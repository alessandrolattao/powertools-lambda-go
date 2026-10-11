package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestCache() (*Cache, *clock) {
	c := NewCache()
	clk := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.SetClock(clk.Now)
	return c, clk
}

func TestCacheExpiry(t *testing.T) {
	t.Parallel()
	c, clk := newTestCache()
	key := Key{Name: "a"}
	c.Store(key, "v", time.Minute)

	clk.Advance(time.Minute)
	got, ok := Lookup[string](c, key)
	assert.True(t, ok, "a value is valid up to and including its expiry")
	assert.Equal(t, "v", got)

	clk.Advance(time.Nanosecond)
	_, ok = c.Lookup(key)
	assert.False(t, ok)
}

func TestCacheZeroMaxAgeStoresNothing(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	c.Store(Key{Name: "a"}, "v", 0)
	c.Store(Key{Name: "b"}, "v", -time.Second)
	_, ok := c.Lookup(Key{Name: "a"})
	assert.False(t, ok)
	_, ok = c.Lookup(Key{Name: "b"})
	assert.False(t, ok)
}

func TestCacheKeysAreDistinct(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	keys := []Key{
		{Name: "a"},
		{Name: "a", Transform: TransformJSON},
		{Name: "a", Nested: true},
		{Name: "a", Variant: "decrypt=true"},
	}
	for i, k := range keys {
		c.Store(k, i, time.Minute)
	}
	for i, k := range keys {
		got, ok := Lookup[int](c, k)
		require.True(t, ok)
		assert.Equal(t, i, got)
	}
	_, ok := Lookup[string](c, keys[0])
	assert.False(t, ok, "a value of another type is not returned")
}

func TestFetchCachesAndForces(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	calls := 0
	fetch := func(context.Context) (string, error) { calls++; return "v", nil }
	ctx := t.Context()
	for range 3 {
		got, err := Fetch(ctx, c, Key{Name: "a"}, time.Minute, false, fetch)
		require.NoError(t, err)
		assert.Equal(t, "v", got)
	}
	assert.Equal(t, 1, calls)

	_, err := Fetch(ctx, c, Key{Name: "a"}, time.Minute, true, fetch)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "force fetches again")

	_, err = Fetch(ctx, c, Key{Name: "b"}, 0, false, fetch)
	require.NoError(t, err)
	_, err = Fetch(ctx, c, Key{Name: "b"}, 0, false, fetch)
	require.NoError(t, err)
	assert.Equal(t, 4, calls, "a zero max age never caches")
}

func TestFetchErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	errStore := errors.New("store down")
	_, err := Fetch(t.Context(), c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) { return "", errStore })
	require.ErrorIs(t, err, errStore)
	got, err := Fetch(t.Context(), c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) { return "v", nil })
	require.NoError(t, err)
	assert.Equal(t, "v", got)
}

func TestFetchSharesConcurrentCalls(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Go(func() {
			got, err := Fetch(t.Context(), c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) {
				calls.Add(1)
				<-release
				return "v", nil
			})
			assert.NoError(t, err)
			results[i] = got
		})
	}
	// Give every goroutine the time to join the flight, then let it land.
	assert.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load())
	for _, got := range results {
		assert.Equal(t, "v", got)
	}
}

func TestFetchWaiterLeavesOnItsOwnContext(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	go func() {
		_, _ = Fetch(context.Background(), c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) { //nolint:errcheck // the leader's result does not matter here
			close(started)
			<-release
			return "v", nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Fetch(ctx, c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) { return "never", nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestFetchRetriesWhenTheLeaderRanOutOfTime(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	leaderCtx, cancelLeader := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancelLeader()
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := Fetch(leaderCtx, c, Key{Name: "a"}, time.Minute, false, func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})
		leaderDone <- err
	}()
	<-started
	got, err := Fetch(t.Context(), c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) { return "mine", nil })
	require.NoError(t, err)
	assert.Equal(t, "mine", got, "a follower whose context is alive fetches again")
	require.ErrorIs(t, <-leaderDone, context.DeadlineExceeded)
}

func TestClearDuringFetchKeepsNothing(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache()
	_, err := Fetch(t.Context(), c, Key{Name: "a"}, time.Minute, false, func(context.Context) (string, error) {
		c.Clear()
		return "stale", nil
	})
	require.NoError(t, err)
	_, ok := c.Lookup(Key{Name: "a"})
	assert.False(t, ok, "a value fetched across a Clear is not cached")
}
