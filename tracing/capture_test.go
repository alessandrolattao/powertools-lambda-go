package tracing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

type order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

type receipt struct {
	OrderID string `json:"orderId"`
	Paid    bool   `json:"paid"`
}

var errDeclined = errors.New("card declined")

func pay(_ context.Context, o order) (receipt, error) {
	if o.Amount <= 0 {
		return receipt{}, errDeclined
	}
	return receipt{OrderID: o.ID, Paid: true}, nil
}

// functionName is the span otellambda names after the function.
const functionName = "payment-fn"

func TestCaptureLambdaHandlerTracesTheInvocation(t *testing.T) {
	t.Setenv(env.LambdaFunctionName, functionName)
	t.Setenv(env.LambdaInitializationType, "on-demand")
	coldstart.Reset()
	t.Cleanup(coldstart.Reset)
	tracer, recorder := recordingTracer(t, WithService("payment"))
	handler := tracer.CaptureLambdaHandler(pay)

	got, err := handler(invocationContext(t, "req-1", testHeader), order{ID: "o-1", Amount: 10})
	require.NoError(t, err)
	assert.Equal(t, receipt{OrderID: "o-1", Paid: true}, got)

	span := spanNamed(t, recorder, functionName)
	assert.Equal(t, trace.SpanKindServer, span.SpanKind())
	assert.Equal(t, testTrace, span.SpanContext().TraceID().String(), "the span joins the trace Lambda started")
	assert.Equal(t, testParent, span.Parent().SpanID().String())
	assert.True(t, span.Parent().IsRemote())

	attrs := attributes(span)
	assert.True(t, attrs[ColdStartAnnotation].AsBool())
	assert.Equal(t, "payment", attrs[ServiceAnnotation].AsString())
	assert.Equal(t, "req-1", attrs["faas.invocation_id"].AsString())
	assert.ElementsMatch(t, []string{ColdStartAnnotation, ServiceAnnotation}, annotationKeys(span))
	assert.Equal(t, map[string]any{"pay response": map[string]any{"orderId": "o-1", "paid": true}},
		metadata(t, span, "payment"))
	assert.Equal(t, codes.Unset, span.Status().Code)

	recorder.Reset()
	_, err = handler(invocationContext(t, "req-2", testHeader), order{ID: "o-2", Amount: 10})
	require.NoError(t, err)
	assert.False(t, attributes(spanNamed(t, recorder, functionName))[ColdStartAnnotation].AsBool(),
		"only the first invocation is cold")
}

func TestCaptureLambdaHandlerWithoutAService(t *testing.T) {
	t.Setenv(env.ServiceName, "")
	t.Setenv(env.LambdaFunctionName, functionName)
	tracer, recorder := recordingTracer(t)

	_, err := tracer.CaptureLambdaHandler(pay)(invocationContext(t, "req-1", testHeader), order{ID: "o-1", Amount: 1})
	require.NoError(t, err)

	span := spanNamed(t, recorder, functionName)
	assert.Equal(t, []string{ColdStartAnnotation}, annotationKeys(span), "no Service annotation without a service")
	assert.NotNil(t, metadata(t, span, DefaultNamespace)["pay response"])
}

func TestCaptureLambdaHandlerFindsTheParent(t *testing.T) {
	const otherHeader = "Root=1-6759e988-bd862e3fe1be46a994272794;Parent=63995c3f42cd8ad8;Sampled=1"
	tests := []struct {
		name      string
		ctxHeader string
		envHeader string
		wantTrace string
	}{
		{name: "the context header", ctxHeader: testHeader, wantTrace: testTrace},
		{
			name:      "the context header beats the environment",
			ctxHeader: testHeader, envHeader: otherHeader, wantTrace: testTrace,
		},
		{name: "the environment as a last resort", envHeader: testHeader, wantTrace: testTrace},
		{name: "a malformed header starts a trace", ctxHeader: "Root=garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(env.XRayTraceID, tt.envHeader)
			t.Setenv(env.LambdaFunctionName, functionName)
			tracer, recorder := recordingTracer(t)

			_, err := tracer.CaptureLambdaHandler(pay)(invocationContext(t, "req", tt.ctxHeader), order{Amount: 1})
			require.NoError(t, err)

			span := spanNamed(t, recorder, functionName)
			if tt.wantTrace == "" {
				assert.False(t, span.Parent().IsValid(), "a root span")
				return
			}
			assert.Equal(t, tt.wantTrace, span.SpanContext().TraceID().String())
		})
	}
}

func TestCaptureLambdaHandlerKeepsAnActiveSpan(t *testing.T) {
	t.Setenv(env.LambdaFunctionName, functionName)
	tracer, recorder := recordingTracer(t)

	ctx, outer := tracer.Provider().Tracer("test").Start(invocationContext(t, "req", testHeader), "outer")
	_, err := tracer.CaptureLambdaHandler(pay)(ctx, order{Amount: 1})
	require.NoError(t, err)
	outer.End()

	assert.Equal(t, outer.SpanContext().SpanID(), spanNamed(t, recorder, functionName).Parent().SpanID())
}

func TestCaptureLambdaHandlerRecordsTheError(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		opts     []CaptureOption
		metadata bool
	}{
		{name: "by default", metadata: true},
		{name: "error capture off", opts: []CaptureOption{WithCaptureError(false)}},
		{name: "POWERTOOLS_TRACER_CAPTURE_ERROR off", env: "false"},
		{name: "the option beats the variable", env: "false", opts: []CaptureOption{WithCaptureError(true)}, metadata: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(env.TracerCaptureErr, tt.env)
			t.Setenv(env.LambdaFunctionName, functionName)
			tracer, recorder := recordingTracer(t, WithService("payment"))

			_, err := tracer.CaptureLambdaHandler(pay, tt.opts...)(invocationContext(t, "req", testHeader), order{})
			require.ErrorIs(t, err, errDeclined, "the handler's error is returned as it is")

			span := spanNamed(t, recorder, functionName)
			assert.Equal(t, codes.Error, span.Status().Code, "the failure shows whatever the capture")
			assert.Equal(t, errDeclined.Error(), span.Status().Description)
			require.Len(t, span.Events(), 1)
			assert.Equal(t, "exception", span.Events()[0].Name)

			meta := metadata(t, span, "payment")
			if tt.metadata {
				assert.Equal(t, map[string]any{"pay error": "card declined"}, meta)
			} else {
				assert.Nil(t, meta)
			}
		})
	}
}

func TestCaptureLambdaHandlerResponseCapture(t *testing.T) {
	tests := []struct {
		name string
		env  string
		opts []CaptureOption
		want bool
	}{
		{name: "by default", want: true},
		{name: "response capture off", opts: []CaptureOption{WithCaptureResponse(false)}},
		{name: "POWERTOOLS_TRACER_CAPTURE_RESPONSE off", env: "0"},
		{name: "the option beats the variable", env: "off", opts: []CaptureOption{WithCaptureResponse(true)}, want: true},
		{name: "an invalid variable keeps the default", env: "maybe", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(env.TracerCaptureResp, tt.env)
			t.Setenv(env.LambdaFunctionName, functionName)
			tracer, recorder := recordingTracer(t, WithService("payment"))

			_, err := tracer.CaptureLambdaHandler(pay, tt.opts...)(invocationContext(t, "req", testHeader), order{Amount: 1})
			require.NoError(t, err)

			_, captured := metadata(t, spanNamed(t, recorder, functionName), "payment")["pay response"]
			assert.Equal(t, tt.want, captured)
		})
	}
}

func TestCaptureLambdaHandlerSkipsWhatItCannotCapture(t *testing.T) {
	t.Setenv(env.LambdaFunctionName, functionName)
	tracer, recorder := recordingTracer(t, WithService("payment"))
	ctx := invocationContext(t, "req", testHeader)

	var none *receipt
	_, err := tracer.CaptureLambdaHandler(func(context.Context, order) (*receipt, error) { return none, nil })(ctx, order{})
	require.NoError(t, err)
	assert.Nil(t, metadata(t, spanNamed(t, recorder, functionName), "payment"), "a nil result is not captured")

	recorder.Reset()
	_, err = tracer.CaptureLambdaHandler(func(context.Context, order) (struct{}, error) { return struct{}{}, nil })(ctx, order{})
	require.NoError(t, err)
	assert.Nil(t, metadata(t, spanNamed(t, recorder, functionName), "payment"), "nor is Go's empty result")

	recorder.Reset()
	stream := strings.NewReader("streamed body")
	got, err := tracer.CaptureLambdaHandler(func(context.Context, order) (io.Reader, error) { return stream, nil })(ctx, order{})
	require.NoError(t, err)
	assert.Nil(t, metadata(t, spanNamed(t, recorder, functionName), "payment"), "a stream is not captured")
	body, err := io.ReadAll(got)
	require.NoError(t, err)
	assert.Equal(t, "streamed body", string(body), "nor consumed")

	recorder.Reset()
	_, err = tracer.CaptureLambdaHandler(func(context.Context, []byte) ([]byte, error) {
		return []byte(`{"Status":"SUCCEEDED"}`), nil
	}, WithName("durable"))(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"Status": "SUCCEEDED"},
		metadata(t, spanNamed(t, recorder, functionName), "payment")["durable response"],
		"a raw JSON response is kept as JSON")
}

func TestCaptureLambdaHandlerRecordsAPanicAndResumesIt(t *testing.T) {
	t.Setenv(env.LambdaFunctionName, functionName)
	tracer, recorder := recordingTracer(t, WithService("payment"))
	handler := tracer.CaptureLambdaHandler(func(context.Context, order) (receipt, error) {
		panic("out of stock")
	})

	assert.PanicsWithValue(t, "out of stock", func() {
		_, _ = handler(invocationContext(t, "req", testHeader), order{}) //nolint:errcheck // the call panics
	})

	span := spanNamed(t, recorder, functionName)
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "panic: out of stock", span.Status().Description)
	require.Len(t, span.Events(), 1)
	var stack string
	for _, kv := range span.Events()[0].Attributes {
		if kv.Key == "exception.stacktrace" {
			stack = kv.Value.AsString()
		}
	}
	assert.Contains(t, stack, "TestCaptureLambdaHandlerRecordsAPanicAndResumesIt", "the stack reaches the panic")
	assert.Contains(t, metadata(t, span, "payment"), "func1 error")
}

func TestCaptureLambdaHandlerOfADisabledTracer(t *testing.T) {
	t.Parallel()
	tracer := New(WithDisabled(true))
	handler := tracer.CaptureLambdaHandler(pay)
	got, err := handler(t.Context(), order{ID: "o", Amount: 1})
	require.NoError(t, err)
	assert.True(t, got.Paid)
}

func TestCaptureLambdaHandlerUnderConcurrentInvocations(t *testing.T) {
	t.Setenv(env.LambdaFunctionName, functionName)
	tracer, recorder := recordingTracer(t, WithService("payment"))
	handler := tracer.CaptureLambdaHandler(func(ctx context.Context, o order) (receipt, error) {
		PutAnnotation(ctx, "order", o.ID)
		return receipt{OrderID: o.ID}, nil
	})

	const invocations = 32
	results := make([]receipt, invocations)
	errs := make([]error, invocations)
	var wg sync.WaitGroup
	for i := range invocations {
		header := fmt.Sprintf("Root=1-5759e988-%024x;Parent=53995c3f42cd8ad8;Sampled=1", i+1)
		ctx := invocationContext(t, "req-"+strconv.Itoa(i), header)
		wg.Go(func() { results[i], errs[i] = handler(ctx, order{ID: strconv.Itoa(i)}) })
	}
	wg.Wait()
	for i := range invocations {
		require.NoError(t, errs[i])
		assert.Equal(t, strconv.Itoa(i), results[i].OrderID)
	}

	spans := recorder.Ended()
	require.Len(t, spans, invocations)
	traces := map[trace.TraceID]bool{}
	for _, span := range spans {
		traces[span.SpanContext().TraceID()] = true
		order := attributes(span)["order"].AsString()
		meta := metadata(t, span, "payment")["func1 response"]
		assert.Equal(t, map[string]any{"orderId": order, "paid": false}, meta, "each span carries its own invocation")
	}
	assert.Len(t, traces, invocations, "each invocation joins its own trace")
}

func charge(_ context.Context, o order) (int, error) {
	if o.Amount <= 0 {
		return 0, errDeclined
	}
	return o.Amount * 100, nil
}

func TestCaptureMethodOpensASubsegment(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t, WithService("payment"))
	captured := tracer.CaptureMethod(charge)

	ctx, parent := tracer.Provider().Tracer("test").Start(t.Context(), "parent")
	cents, err := captured(ctx, order{Amount: 3})
	parent.End()
	require.NoError(t, err)
	assert.Equal(t, 300, cents)

	span := spanNamed(t, recorder, "## tracing.charge")
	assert.Equal(t, trace.SpanKindInternal, span.SpanKind())
	assert.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
	assert.Equal(t, map[string]any{"tracing.charge response": float64(300)}, metadata(t, span, "payment"))
	assert.Empty(t, annotationKeys(span), "no cold start annotation on a method")

	_, err = captured(t.Context(), order{})
	require.ErrorIs(t, err, errDeclined)
}

type service struct{ fee int }

func (s *service) Charge(_ context.Context, o order) (int, error) { return o.Amount + s.fee, nil }

func TestCaptureMethodNames(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	svc := &service{fee: 1}

	_, err := tracer.CaptureMethod(svc.Charge)(t.Context(), order{})
	require.NoError(t, err)
	_, err = tracer.CaptureMethod(func(context.Context, int) (int, error) { return 0, nil })(t.Context(), 0)
	require.NoError(t, err)
	_, err = tracer.CaptureMethod(charge, WithName("charge card"))(t.Context(), order{Amount: 1})
	require.NoError(t, err)

	names := make([]string, 0, len(recorder.Ended()))
	for _, span := range recorder.Ended() {
		names = append(names, span.Name())
	}
	assert.Equal(t, []string{
		"## tracing.service.Charge",
		"## tracing.TestCaptureMethodNames.func1",
		"## charge card",
	}, names)
}

func TestCaptureRunsABlockInASpan(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t, WithService("payment"))

	got, err := tracer.Capture(t.Context(), "fetch (cached)", func(ctx context.Context) (string, error) {
		PutAnnotation(ctx, "cache", "hit")
		return "value", nil
	}, WithCaptureResponse(false))
	require.NoError(t, err)
	assert.Equal(t, "value", got)

	span := spanNamed(t, recorder, "## fetch cached")
	assert.Equal(t, "hit", attributes(span)["cache"].AsString(), "the block gets the span's context")
	assert.Nil(t, metadata(t, span, "payment"))
}

func TestCaptureRecordsAPanic(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	errBoom := errors.New("boom")

	assert.PanicsWithError(t, "boom", func() {
		_, _ = tracer.Capture(t.Context(), "explode", func(context.Context) (int, error) { //nolint:errcheck // the call panics
			panic(errBoom)
		})
	})
	span := spanNamed(t, recorder, "## explode")
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "panic: boom", span.Status().Description)
}

func TestCaptureOfADisabledTracer(t *testing.T) {
	t.Parallel()
	tracer := New(WithDisabled(true))

	got, err := tracer.Capture(t.Context(), "noop", func(ctx context.Context) (bool, error) {
		return trace.SpanFromContext(ctx).IsRecording(), nil
	})
	require.NoError(t, err)
	assert.False(t, got)

	cents, err := tracer.CaptureMethod(charge)(t.Context(), order{Amount: 2})
	require.NoError(t, err)
	assert.Equal(t, 200, cents)
}
