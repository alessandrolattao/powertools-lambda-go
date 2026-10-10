package middleware

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type order struct{ ID string }

func echo(_ context.Context, o order) (string, error) { return o.ID, nil }

// recordingMiddleware appends its name to calls on the way in and on the way out.
func recordingMiddleware[E, R any](name string, calls *[]string) Middleware[E, R] {
	return New(name, func(ctx context.Context, event E, next Handler[E, R]) (R, error) {
		*calls = append(*calls, "before "+name)
		result, err := next(ctx, event)
		*calls = append(*calls, "after "+name)
		return result, err
	}, WithTracing(false))
}

func TestChainOrder(t *testing.T) {
	t.Parallel()
	var calls []string
	h := Chain(echo, recordingMiddleware[order, string]("outer", &calls), recordingMiddleware[order, string]("inner", &calls))

	got, err := h(t.Context(), order{ID: "42"})
	require.NoError(t, err)
	assert.Equal(t, "42", got)
	assert.Equal(t, []string{"before outer", "before inner", "after inner", "after outer"}, calls)
}

// upper is a middleware that works for any event, as a generic function.
func upper[E any](next Handler[E, string]) Handler[E, string] {
	return func(ctx context.Context, event E) (string, error) {
		result, err := next(ctx, event)
		return "<" + result + ">", err
	}
}

func TestChainInstantiatesGenericMiddlewares(t *testing.T) {
	t.Parallel()
	h := Chain(echo, upper)
	got, err := h(t.Context(), order{ID: "7"})
	require.NoError(t, err)
	assert.Equal(t, "<7>", got)
}

func TestNewCanShortCircuit(t *testing.T) {
	t.Parallel()
	cached := New("cache", func(context.Context, order, Handler[order, string]) (string, error) {
		return "from cache", nil
	}, WithTracing(false))
	got, err := Chain(func(context.Context, order) (string, error) {
		t.Fatal("the handler must not run")
		return "", nil
	}, cached)(t.Context(), order{})
	require.NoError(t, err)
	assert.Equal(t, "from cache", got)
}

func TestBeforeStopsOnError(t *testing.T) {
	t.Parallel()
	errDenied := errors.New("denied")
	h := Chain(echo, Before[order, string](func(_ context.Context, o order) error {
		if o.ID == "" {
			return errDenied
		}
		return nil
	}))
	_, err := h(t.Context(), order{})
	require.ErrorIs(t, err, errDenied)

	got, err := h(t.Context(), order{ID: "1"})
	require.NoError(t, err)
	assert.Equal(t, "1", got)
}

func TestAfterSeesTheResult(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	h := Chain(func(context.Context, order) (string, error) { return "partial", errBoom },
		After(func(_ context.Context, _ order, result string, err error) (string, error) {
			return result + " recovered", errors.Unwrap(errors.Join(nil, err))
		}))
	got, err := h(t.Context(), order{})
	require.NoError(t, err)
	assert.Equal(t, "partial recovered", got)
}

func recordingProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	return tp, recorder
}

func TestNewTracesExecution(t *testing.T) {
	t.Parallel()
	tp, recorder := recordingProvider(t)

	errBoom := errors.New("boom")
	failing := New("validate", func(context.Context, order, Handler[order, string]) (string, error) {
		return "", errBoom
	}, WithTracing(true), WithTracerProvider(tp))
	_, err := Chain(echo, failing)(t.Context(), order{})
	require.ErrorIs(t, err, errBoom)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "## validate", spans[0].Name())
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestTracingFollowsTheEnvironment(t *testing.T) {
	tp, recorder := recordingProvider(t)

	t.Setenv("POWERTOOLS_TRACE_MIDDLEWARES", "true")
	passThrough := func(ctx context.Context, o order, next Handler[order, string]) (string, error) {
		return next(ctx, o)
	}
	_, err := Chain(echo, New("traced", passThrough, WithTracerProvider(tp)))(t.Context(), order{ID: "x"})
	require.NoError(t, err)

	t.Setenv("POWERTOOLS_TRACE_MIDDLEWARES", "false")
	_, err = Chain(echo, New("silent", passThrough, WithTracerProvider(tp)))(t.Context(), order{ID: "x"})
	require.NoError(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "## traced", spans[0].Name())
}
