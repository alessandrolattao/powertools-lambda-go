// Package middleware builds handler middlewares: logic that runs before and
// after each invocation of a Lambda handler, the Go side of Powertools'
// middleware factory.
//
// A handler is any function aws-lambda-go's lambda.Start accepts in the
// (context, event) -> (result, error) shape; a middleware takes a handler and
// returns one that wraps it. Every handler-level Powertools utility (the
// logger's context injection, the tracer's handler capture, metrics flushing,
// idempotency) is such a middleware, so they stack with each other and with
// your own:
//
//	lambda.Start(middleware.Chain(handle,
//		middleware.New("audit", audit),
//		logResponse,
//	))
//
// A middleware that does not care about the event type is a generic
// function, such as logResponse above, and Chain instantiates it for the
// handler at hand.
package middleware

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// instrumentation names the spans this package starts.
const instrumentation = "github.com/alessandrolattao/powertools-lambda-go/middleware"

// Handler is a Lambda handler that takes an event E and returns a result R.
type Handler[E, R any] = func(ctx context.Context, event E) (R, error)

// Middleware wraps a Handler in another one.
type Middleware[E, R any] = func(next Handler[E, R]) Handler[E, R]

// Func is the body of a middleware made with New: it receives the event and
// the next handler, and decides whether, when and how to call it.
type Func[E, R any] = func(ctx context.Context, event E, next Handler[E, R]) (R, error)

// Option configures a middleware made with New.
type Option func(*config)

type config struct {
	traced   bool
	provider trace.TracerProvider
}

// WithTracing records every run of the middleware as a span named
// "## <name>", so that its overhead shows in the trace. It defaults to the
// POWERTOOLS_TRACE_MIDDLEWARES environment variable. The spans go to the
// global OpenTelemetry tracer provider, which the tracing package sets up.
func WithTracing(enabled bool) Option {
	return func(c *config) { c.traced = enabled }
}

// WithTracerProvider sends the spans of WithTracing to tp instead of the
// global tracer provider.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.provider = tp }
}

// New makes a Middleware out of fn. The name identifies it in traces and in
// the debug log.
func New[E, R any](name string, fn Func[E, R], opts ...Option) Middleware[E, R] {
	cfg := config{traced: env.Bool(env.TraceMiddlewares, false)}
	for _, opt := range opts {
		opt(&cfg)
	}
	return func(next Handler[E, R]) Handler[E, R] {
		return func(ctx context.Context, event E) (R, error) {
			return run(ctx, name, &cfg, event, next, fn)
		}
	}
}

func run[E, R any](ctx context.Context, name string, cfg *config, event E, next Handler[E, R], fn Func[E, R]) (R, error) {
	if !cfg.traced {
		result, err := fn(ctx, event, next)
		logFailure(ctx, name, err)
		return result, err
	}
	provider := cfg.provider
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	ctx, span := provider.Tracer(instrumentation).Start(ctx, "## "+name)
	defer span.End()
	result, err := fn(ctx, event, next)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	logFailure(ctx, name, err)
	return result, err
}

func logFailure(ctx context.Context, name string, err error) {
	if err != nil {
		debug.Logger().DebugContext(ctx, "middleware returned an error", "middleware", name, "err", err)
	}
}

// Chain applies middlewares to h. The first middleware is the outermost: it
// runs first on the way in and last on the way out.
func Chain[E, R any](h Handler[E, R], middlewares ...Middleware[E, R]) Handler[E, R] {
	for _, mw := range slices.Backward(middlewares) {
		h = mw(h)
	}
	return h
}

// Before makes a middleware that runs fn before the handler. An error from fn
// stops the invocation before the handler runs.
func Before[E, R any](fn func(ctx context.Context, event E) error) Middleware[E, R] {
	return func(next Handler[E, R]) Handler[E, R] {
		return func(ctx context.Context, event E) (R, error) {
			if err := fn(ctx, event); err != nil {
				var zero R
				return zero, err
			}
			return next(ctx, event)
		}
	}
}

// After makes a middleware that runs fn after the handler, with what the
// handler returned; fn returns what the invocation returns.
func After[E, R any](fn func(ctx context.Context, event E, result R, err error) (R, error)) Middleware[E, R] {
	return func(next Handler[E, R]) Handler[E, R] {
		return func(ctx context.Context, event E) (R, error) {
			result, err := next(ctx, event)
			return fn(ctx, event, result, err)
		}
	}
}
