package appsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// BatchOption configures a BatchItemResolver.
type BatchOption func(*batchConfig)

type batchConfig struct {
	raiseOnError bool
	concurrency  int
}

// WithRaiseOnError decides what happens when the resolver fails for one
// event of the batch. Off (the default), the event's result is null, which
// AppSync reports as an error for that field only, and the other events are
// resolved. On, the first failure fails the whole invocation, through the
// OnError handlers, as Powertools Python's raise_on_error=True.
func WithRaiseOnError(raise bool) BatchOption {
	return func(c *batchConfig) { c.raiseOnError = raise }
}

// WithConcurrency resolves up to n events of the batch at the same time,
// each in its own goroutine; the default, 1, resolves them one after the
// other. It is Powertools Python's async_batch_resolver, which gathers the
// events concurrently, with a bound. The results keep the order of the
// batch either way. With WithRaiseOnError, the first failure cancels the
// context of the events still running and no further event starts.
func WithConcurrency(n int) BatchOption {
	return func(c *batchConfig) { c.concurrency = max(n, 1) }
}

// BatchItemResolver registers fn as the resolver of the field fieldName of
// the type typeName for batch invocations, called once per event: Powertools
// Python's batch_resolver with aggregate=False. The results are returned in
// the order of the batch. It moves the N+1 problem from AppSync into the
// function rather than solving it; prefer BatchResolver when the data
// source can fetch the whole batch at once.
//
// By default a failing event (an error from fn, or arguments that do not
// decode into A) gets a null result and the others are resolved; see
// WithRaiseOnError and WithConcurrency. A panic in fn is not recovered.
//
// It panics as Resolver does.
func BatchItemResolver[A, R any](r *Router, typeName, fieldName string, fn func(ctx context.Context, event Event[A]) (R, error), opts ...BatchOption) {
	cfg := batchConfig{concurrency: 1}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	r.addBatch("BatchItemResolver", typeName, fieldName, func(ctx context.Context, items []item) (jsontext.Value, error) {
		results := make([]jsontext.Value, len(items))
		err := forEach(ctx, len(items), cfg.concurrency, func(ctx context.Context, i int) error {
			out, err := resolveItem(ctx, &items[i], fn)
			if err == nil {
				results[i] = out
				return nil
			}
			if cfg.raiseOnError {
				return err
			}
			debug.Logger().DebugContext(ctx, "failed to process a batch item; its result is null",
				"index", i, "field", fieldName, "err", err)
			results[i] = jsontext.Value("null")
			return nil
		})
		if err != nil {
			return nil, err
		}
		out, err := json.Marshal(results)
		if err != nil {
			return nil, fmt.Errorf("appsync: encode the batch results: %w", err)
		}
		return out, nil
	})
}

func resolveItem[A, R any](ctx context.Context, it *item, fn func(ctx context.Context, event Event[A]) (R, error)) (jsontext.Value, error) {
	ev, err := typed[A](it)
	if err != nil {
		return nil, err
	}
	result, err := fn(ctx, ev)
	if err != nil {
		return nil, err
	}
	return encode(result, it.event.ParentType(), it.event.Field())
}

// forEach calls fn for every index below n, one after the other or up to
// limit at a time, and stops at the first error, which it returns: the
// calls still running see their context cancelled, and no other starts.
func forEach(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	if limit > 1 {
		return forEachConcurrent(ctx, n, limit, fn)
	}
	for i := range n {
		if err := fn(ctx, i); err != nil {
			return err
		}
	}
	return nil
}

func forEachConcurrent(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		wg sync.WaitGroup
		g  group
	)
	slots := make(chan struct{}, limit)
	for i := range n {
		slots <- struct{}{}
		if g.stopped() {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			defer func() {
				if v := recover(); v != nil {
					g.panic(v)
					cancel(context.Canceled)
				}
			}()
			if err := fn(ctx, i); err != nil {
				g.fail(err)
				cancel(err)
			}
		})
	}
	wg.Wait()
	return g.result()
}

// group records the first failure of concurrent calls: an error, or a panic
// that result raises again in the caller's goroutine, so that it reaches
// aws-lambda-go as a panic of the handler rather than crashing the process.
type group struct {
	mu       sync.Mutex
	err      error
	panicked bool
	panicVal any
}

func (g *group) stopped() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err != nil || g.panicked
}

func (g *group) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err == nil {
		g.err = err
	}
}

func (g *group) panic(v any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.panicked {
		g.panicked, g.panicVal = true, v
	}
}

func (g *group) result() error {
	if g.panicked {
		panic(g.panicVal)
	}
	return g.err
}
