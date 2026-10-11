package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// Key identifies a cached value: the name (or path, or joined names) it was
// read under, the transform applied to it, whether it holds several values,
// and the variant the store options made of it.
type Key struct {
	Name      string
	Transform string
	Nested    bool
	Variant   string
}

func (k Key) flight() string {
	return k.Name + "\x00" + k.Transform + "\x00" + strconv.FormatBool(k.Nested) + "\x00" + k.Variant
}

type entry struct {
	value   any
	expires time.Time
}

// Cache keeps values for a time, safely for concurrent use, and makes
// concurrent reads of a missing value share one fetch.
type Cache struct {
	mu         sync.Mutex
	entries    map[Key]entry
	generation uint64
	now        func() time.Time
	flights    singleflight.Group
}

// NewCache returns an empty cache on the wall clock. time.Now carries a
// monotonic reading, so expiry is immune to clock changes.
func NewCache() *Cache {
	return &Cache{entries: make(map[Key]entry), now: time.Now}
}

// SetClock replaces the clock, for tests.
func (c *Cache) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// Lookup returns the value cached under key, if it has not expired. A value
// is valid up to and including its expiry instant, as in Python.
func (c *Cache) Lookup(key Key) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.value, true
}

// Store caches value under key for maxAge. A max age of zero or less
// caches nothing, as in Python.
func (c *Cache) Store(key Key, value any, maxAge time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, value, maxAge)
}

func (c *Cache) storeLocked(key Key, value any, maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	c.entries[key] = entry{value: value, expires: c.now().Add(maxAge)}
}

// storeIf caches value unless the cache was cleared since generation: a
// fetch that started before ClearCache must not bring old values back.
func (c *Cache) storeIf(generation uint64, key Key, value any, maxAge time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == generation {
		c.storeLocked(key, value, maxAge)
	}
}

// Clear empties the cache.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	c.generation++
}

func (c *Cache) currentGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// Fetch returns the value of type V cached under key or, when there is none, when it
// has expired, or when force is set, the value fetch returns, which it then
// caches for maxAge. Concurrent fetches of one key share a single call.
//
// The shared call runs on a context detached from the cancellation of the
// caller that started it (it keeps that caller's deadline and values), and
// every caller waits only as long as its own context allows: invocations
// served by one process (Lambda Managed Instances) do not fail because
// another one ended.
func Fetch[V any](ctx context.Context, c *Cache, key Key, maxAge time.Duration, force bool, fetch func(context.Context) (V, error)) (V, error) {
	if !force {
		if value, ok := Lookup[V](c, key); ok {
			return value, nil
		}
	}
	generation := c.currentGeneration()
	// led is set when this caller's function is the shared call: the
	// singleflight runs only the first caller's.
	var led atomic.Bool
	results := c.flights.DoChan(key.flight(), func() (any, error) {
		led.Store(true)
		shared, cancel := detach(ctx)
		defer cancel()
		value, err := fetch(shared)
		if err == nil {
			c.storeIf(generation, key, value, maxAge)
		}
		return value, err
	})
	var zero V
	select {
	case <-ctx.Done():
		return zero, fmt.Errorf("wait for the value: %w", context.Cause(ctx))
	case res := <-results:
		if res.Err != nil && isContextError(res.Err) && ctx.Err() == nil && !led.Load() {
			// The call ran out of the time of the caller that started it,
			// not ours: try again on our own context. The caller that
			// started it does not: its detached context's timer may fire a
			// moment before its own context notices the same deadline.
			value, err := fetch(ctx)
			if err == nil {
				c.storeIf(generation, key, value, maxAge)
			}
			return value, err
		}
		return typed[V](res)
	}
}

// typed returns the result of a shared fetch as a V.
func typed[V any](res singleflight.Result) (V, error) {
	var zero V
	if res.Err != nil {
		return zero, res.Err
	}
	value, ok := res.Val.(V)
	if !ok {
		return zero, fmt.Errorf("%w: %T", ErrCacheType, res.Val)
	}
	return value, nil
}

// ErrCacheType reports a cached value of another type than the one asked
// for: two readers used one key for different things, a bug.
var ErrCacheType = errors.New("cached value has an unexpected type")

// Lookup returns the value of type V cached under key, if it has not
// expired.
func Lookup[V any](c *Cache, key Key) (V, bool) {
	value, ok := c.Lookup(key)
	if !ok {
		var zero V
		return zero, false
	}
	typed, ok := value.(V)
	return typed, ok
}

// detach returns a context with the values and the deadline of ctx, but not
// its cancellation.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(detached, deadline)
	}
	return detached, func() {}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// SingleKey is the key of one value read with o.
func SingleKey(name string, o *Options) Key {
	return Key{Name: name, Transform: o.Transform, Variant: o.Variant()}
}

// MultipleKey is the key of the values under path read with o.
func MultipleKey(path string, o *Options) Key {
	return Key{Name: path, Transform: o.Transform, Nested: true, Variant: o.Variant()}
}

// CacheHolder is implemented by the stores of this module that read in
// batches (SSM by name, Secrets Manager by name): they bring the cache their
// provider must share with them. Only this module can implement it.
type CacheHolder interface {
	Cache() *Cache
}
