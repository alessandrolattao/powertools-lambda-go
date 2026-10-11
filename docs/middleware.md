# Middleware factory

Package `middleware` builds handler middlewares: logic that runs before and
after each invocation of a Lambda handler. It is the Go side of Powertools'
`lambda_handler_decorator`.

## Key features

- Run logic before and after the handler, change the event or the result,
  or answer without calling the handler.
- Middlewares that work for any event type, as plain generic functions.
- Trace every middleware as its own span, to see the overhead it adds.

## Getting started

A handler is any `func(context.Context, E) (R, error)`, the shape
`lambda.Start` accepts. A middleware takes a handler and returns one.
`middleware.New` builds one from a function that receives the event and the
next handler:

```go
audit := middleware.New("audit", func(ctx context.Context, e events.SQSEvent, next middleware.Handler[events.SQSEvent, events.SQSEventResponse]) (events.SQSEventResponse, error) {
	slog.InfoContext(ctx, "batch received", "records", len(e.Records))
	return next(ctx, e)
})

lambda.Start(middleware.Chain(handle, audit))
```

`Chain` applies middlewares to a handler: the first is the outermost, so it
runs first on the way in and last on the way out.

### Middlewares for any event

A middleware that does not care about the event type is a generic function;
`Chain` instantiates it for the handler at hand:

```go
func logResponse[E, R any](next middleware.Handler[E, R]) middleware.Handler[E, R] {
	return func(ctx context.Context, event E) (R, error) {
		result, err := next(ctx, event)
		slog.InfoContext(ctx, "handler returned", "result", result, "err", err)
		return result, err
	}
}

lambda.Start(middleware.Chain(handle, logResponse))
```

### Before and after

`Before` runs a function before the handler (an error stops the invocation);
`After` sees, and may replace, what the handler returned.

## Advanced

### Tracing middlewares

`WithTracing(true)`, or `POWERTOOLS_TRACE_MIDDLEWARES=true`, records every
run of the middleware as a span named `## <name>`, through the global
OpenTelemetry tracer provider that the `tracing` package sets up
(`WithTracerProvider` picks another).

### The Powertools decorators

The handler-level utilities are decorators of the same shape, so they stack
with each other and with your own:

```go
h := logger.InjectContext(tracer.CaptureLambdaHandler(metrics.LogMetrics(handle)))
lambda.Start(middleware.Chain(h, audit))
```

## Testing your code

A middleware is a function: wrap a fake handler with it and call the result
with the event your test builds.
