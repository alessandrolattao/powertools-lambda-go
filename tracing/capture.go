package tracing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/aws/aws-lambda-go/lambda"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-lambda-go/otellambda"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// The annotations CaptureLambdaHandler adds to every invocation, as
// Powertools Python names them.
const (
	ColdStartAnnotation = "ColdStart"
	ServiceAnnotation   = "Service"
)

// errPanic marks the error a captured function's panic is recorded as.
var errPanic = errors.New("panic")

// CaptureOption configures CaptureLambdaHandler, CaptureMethod and Capture.
type CaptureOption func(*capture)

// capture is what a decorator knows about the function it wraps.
type capture struct {
	name            string
	captureResponse bool
	captureError    bool
	service         string
}

// WithCaptureResponse decides whether the function's result is added to the
// span as metadata ("<name> response"). It defaults to
// POWERTOOLS_TRACER_CAPTURE_RESPONSE, true when unset. Turn it off for
// results that are sensitive, or too large for a span.
func WithCaptureResponse(enabled bool) CaptureOption {
	return func(c *capture) { c.captureResponse = enabled }
}

// WithCaptureError decides whether the error a function returns, or its
// panic, is added to the span as metadata ("<name> error"). It defaults to
// POWERTOOLS_TRACER_CAPTURE_ERROR, true when unset. The span records the
// error and the failed status either way, as X-Ray records the exception of
// a Powertools Python subsegment either way.
func WithCaptureError(enabled bool) CaptureOption {
	return func(c *capture) { c.captureError = enabled }
}

// WithName names the captured function, in place of the name of the Go
// function: the span of CaptureMethod is "## <name>", and the metadata keys
// are "<name> response" and "<name> error".
func WithName(name string) CaptureOption {
	return func(c *capture) { c.name = name }
}

// newCapture resolves the options of one decorated function, reading the
// environment once, at decoration, as Powertools Python does.
func (t *Tracer) newCapture(name string, opts []CaptureOption) *capture {
	c := &capture{
		name:            name,
		captureResponse: env.Bool(env.TracerCaptureResp, true),
		captureError:    env.Bool(env.TracerCaptureErr, true),
		service:         t.service,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.name = SanitizeSpanName(c.name)
	return c
}

// namespace is where the captured response and error go: the service, as in
// Powertools Python, or X-Ray's default namespace without one.
func (c *capture) namespace() string {
	if c.service == "" {
		return DefaultNamespace
	}
	return c.service
}

// CaptureLambdaHandler traces every invocation of h. The invocation is the
// server span otellambda opens, named after the function, whose parent is the
// trace Lambda started for the invocation: the X-Ray header aws-lambda-go
// puts in the context (it sets _X_AMZN_TRACE_ID only while the environment
// runs one invocation at a time), else _X_AMZN_TRACE_ID. A span already
// active in the context is kept as the parent.
//
// On that span the decorator adds what Powertools Python adds to its
// "## handler" subsegment:
//
//   - the ColdStart annotation, true for the invocation that paid for the
//     execution environment's initialization;
//   - the Service annotation, when the tracer has a service;
//   - the result as metadata, "<handler> response" in the service's
//     namespace (WithCaptureResponse);
//   - a returned error, or a panic, recorded on the span with the failed
//     status, and as metadata "<handler> error" (WithCaptureError). A panic
//     is recorded with its stack trace and then resumed.
//
// The spans are flushed when the invocation ends, before Lambda can freeze
// the environment. A disabled tracer returns h as it is.
func (t *Tracer) CaptureLambdaHandler[E, R any](h middleware.Handler[E, R], opts ...CaptureOption) middleware.Handler[E, R] {
	if t.disabled {
		return h
	}
	c := t.newCapture(handlerName(h), opts)

	// otellambda wraps the raw handler interface; the typed event and result
	// travel through the context, so nothing is encoded twice.
	traced := otellambda.WrapHandler(rawHandler(func(ctx context.Context, _ []byte) ([]byte, error) {
		inv, ok := ctx.Value(invocationKey{}).(*invocation[E, R])
		if !ok {
			return nil, errNoInvocation
		}
		inv.result, inv.err = runHandler(ctx, c, h, inv.event)
		return nil, inv.err
	}), t.lambda...)

	return func(ctx context.Context, event E) (R, error) {
		inv := &invocation[E, R]{event: event}
		ctx = context.WithValue(withLambdaParent(ctx), invocationKey{}, inv)
		if _, err := traced.Invoke(ctx, nil); err != nil && inv.err == nil {
			// Only the instrumentation itself fails this way, before h runs.
			return inv.result, fmt.Errorf("tracing: %w", err)
		}
		return inv.result, inv.err
	}
}

// invocation carries one call of a decorated handler through otellambda.
type invocation[E, R any] struct {
	event  E
	result R
	err    error
}

type invocationKey struct{}

var errNoInvocation = errors.New("tracing: the invocation is missing from the context")

// rawHandler adapts a function to aws-lambda-go's raw handler interface.
type rawHandler func(ctx context.Context, payload []byte) ([]byte, error)

var _ lambda.Handler = rawHandler(nil)

func (f rawHandler) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	return f(ctx, payload)
}

// runHandler calls h inside the invocation span ctx carries.
func runHandler[E, R any](ctx context.Context, c *capture, h middleware.Handler[E, R], event E) (result R, err error) {
	span := trace.SpanFromContext(ctx)
	ctx = withService(ctx, c.service)

	annotate(span, attribute.Bool(ColdStartAnnotation, coldstart.Is(ctx)))
	if c.service != "" {
		annotate(span, attribute.String(ServiceAnnotation, c.service))
	}

	defer c.recordPanic(span)
	result, err = h(ctx, event)
	c.record(ctx, span, result, err)
	return result, err
}

// CaptureMethod traces every call of fn as a span named "## <name>", a
// subsegment in X-Ray, where name is fn's package and function name
// ("orders.charge") unless WithName gives one. The span records fn's result
// and error as CaptureLambdaHandler does, without the ColdStart and Service
// annotations. A disabled tracer returns fn as it is.
func (t *Tracer) CaptureMethod[In, Out any](fn func(context.Context, In) (Out, error), opts ...CaptureOption) func(context.Context, In) (Out, error) {
	if t.disabled {
		return fn
	}
	c := t.newCapture(methodName(fn), opts)
	return func(ctx context.Context, in In) (Out, error) {
		return runCaptured(ctx, t.tracer, c, func(ctx context.Context) (Out, error) {
			return fn(ctx, in)
		})
	}
}

// Capture runs fn in a span named "## <name>", as CaptureMethod does for a
// function, for code that is not a function of its own: a block, a closure,
// a durable step. fn receives the context that carries the span, for the
// calls it makes to nest under it.
func (t *Tracer) Capture[Out any](ctx context.Context, name string, fn func(ctx context.Context) (Out, error), opts ...CaptureOption) (Out, error) {
	if t.disabled {
		return fn(ctx)
	}
	return runCaptured(ctx, t.tracer, t.newCapture(name, opts), fn)
}

func runCaptured[Out any](ctx context.Context, tracer trace.Tracer, c *capture, fn func(ctx context.Context) (Out, error)) (out Out, err error) {
	ctx, span := tracer.Start(ctx, "## "+c.name, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	ctx = withService(ctx, c.service)

	defer c.recordPanic(span)
	out, err = fn(ctx)
	c.record(ctx, span, out, err)
	return out, err
}

// record adds the outcome of the captured function to span.
func (c *capture) record(ctx context.Context, span trace.Span, result any, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if c.captureError {
			c.putMetadata(span, c.name+" error", err.Error())
		}
		debug.Logger().DebugContext(ctx, "captured function failed", "function", c.name, "err", err)
		return
	}
	if c.captureResponse && capturable(result) {
		c.putMetadata(span, c.name+" response", result)
	}
}

// recordPanic records the panic of the captured function on span, then
// resumes it: a panic is a bug, and the tracer does not hide it. It must be
// deferred directly, for recover to see the panic.
func (c *capture) recordPanic(span trace.Span) {
	r := recover() //nolint:revive // recordPanic is itself the deferred function
	if r == nil {
		return
	}
	err := fmt.Errorf("%w: %v", errPanic, r)
	if cause, ok := r.(error); ok {
		err = fmt.Errorf("%w: %w", errPanic, cause)
	}
	span.RecordError(err, trace.WithStackTrace(true))
	span.SetStatus(codes.Error, err.Error())
	if c.captureError {
		c.putMetadata(span, c.name+" error", err.Error())
	}
	panic(r)
}

func (c *capture) putMetadata(span trace.Span, key string, value any) {
	encoded, ok := encodeMetadata(value)
	if !ok || !putMetadata(span, c.namespace(), key, encoded) {
		debug.Logger().Debug("captured metadata dropped", "function", c.name, "key", key)
	}
}

// capturable reports whether a result is worth adding as metadata: not a nil
// value, which Powertools Python skips too, nor a struct{}, Go's "no result",
// and not a stream, which encoding would consume.
func capturable(result any) bool {
	if result == nil {
		return false
	}
	if _, ok := result.(io.Reader); ok {
		return false
	}
	v := reflect.ValueOf(result)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return !v.IsNil()
	case reflect.Struct:
		return v.NumField() > 0
	default:
		return true
	}
}
