package tracing

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// spanContext returns a context carrying a recording span, and a function
// that ends it and returns it as recorded.
func spanContext(t *testing.T) (ctx context.Context, end func() sdktrace.ReadOnlySpan) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := tp.Tracer("test").Start(t.Context(), "span")
	end = func() sdktrace.ReadOnlySpan {
		span.End()
		ended := recorder.Ended()
		require.Len(t, ended, 1)
		return ended[0]
	}
	return ctx, end
}

type status string

func TestPutAnnotationTypes(t *testing.T) {
	t.Parallel()
	ctx, end := spanContext(t)

	PutAnnotation(ctx, "text", "hello")
	PutAnnotation(ctx, "named", status("PAID"))
	PutAnnotation(ctx, "flag", true)
	PutAnnotation(ctx, "count", 42)
	PutAnnotation(ctx, "small", int8(-3))
	PutAnnotation(ctx, "unsigned", uint32(7))
	PutAnnotation(ctx, "huge", uint64(math.MaxUint64))
	PutAnnotation(ctx, "ratio", 0.5)
	PutAnnotation(ctx, "single", float32(1.5))
	PutAnnotation(ctx, "nan", math.NaN())
	PutAnnotation(ctx, "inf", math.Inf(1))
	PutAnnotation(ctx, "", "no key")

	span := end()
	attrs := attributes(span)
	assert.Equal(t, "hello", attrs["text"].AsString())
	assert.Equal(t, "PAID", attrs["named"].AsString())
	assert.True(t, attrs["flag"].AsBool())
	assert.Equal(t, int64(42), attrs["count"].AsInt64())
	assert.Equal(t, int64(-3), attrs["small"].AsInt64())
	assert.Equal(t, int64(7), attrs["unsigned"].AsInt64())
	assert.InDelta(t, float64(math.MaxUint64), attrs["huge"].AsFloat64(), 1)
	assert.InDelta(t, 0.5, attrs["ratio"].AsFloat64(), 0)
	assert.InDelta(t, 1.5, attrs["single"].AsFloat64(), 0)
	assert.NotContains(t, attrs, attribute.Key("nan"), "X-Ray cannot store NaN")
	assert.NotContains(t, attrs, attribute.Key("inf"))
	assert.Equal(t,
		[]string{"text", "named", "flag", "count", "small", "unsigned", "huge", "ratio", "single"},
		annotationKeys(span))
}

func TestPutAnnotationSanitizesKeys(t *testing.T) {
	t.Parallel()
	ctx, end := spanContext(t)

	PutAnnotation(ctx, "order.id", "o-1")
	PutAnnotation(ctx, "payment status", "PAID")
	PutAnnotation(ctx, "città", "Roma")

	span := end()
	assert.Equal(t, []string{"order_id", "payment_status", "citt_"}, annotationKeys(span))
	assert.Equal(t, "o-1", attributes(span)["order_id"].AsString())
}

func TestPutAnnotationReplacesAValue(t *testing.T) {
	t.Parallel()
	ctx, end := spanContext(t)

	PutAnnotation(ctx, "attempt", 1)
	PutAnnotation(ctx, "attempt", 2)

	span := end()
	assert.Equal(t, []string{"attempt"}, annotationKeys(span))
	assert.Equal(t, int64(2), attributes(span)["attempt"].AsInt64())
}

func TestPutAnnotationFromManyGoroutines(t *testing.T) {
	t.Parallel()
	ctx, end := spanContext(t)

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() { PutAnnotation(ctx, "key"+strconv.Itoa(i), i) })
	}
	wg.Wait()

	assert.Len(t, annotationKeys(end()), 100, "no annotation is lost to a concurrent one")
}

func TestPutWithoutASpanDoesNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	PutAnnotation(ctx, "key", "value")
	PutMetadata(ctx, "key", "value", "")
	assert.False(t, trace.SpanFromContext(ctx).IsRecording())
}

type invoice struct {
	Number string   `json:"number"`
	Lines  []string `json:"lines"`
}

func TestPutMetadataNamespaces(t *testing.T) {
	t.Setenv(env.ServiceName, "")
	ctx, end := spanContext(t)

	PutMetadata(ctx, "invoice", invoice{Number: "F-1", Lines: []string{"a", "b"}}, "billing")
	PutMetadata(ctx, "total", 12.5, "billing")
	PutMetadata(ctx, "note", "plain", "")
	PutMetadata(ctx, "hidden", "nope", "AWS.reserved")

	span := end()
	assert.Equal(t, map[string]any{
		"invoice": map[string]any{"number": "F-1", "lines": []any{"a", "b"}},
		"total":   12.5,
	}, metadata(t, span, "billing"))
	assert.Equal(t, map[string]any{"note": "plain"}, metadata(t, span, DefaultNamespace),
		"no namespace and no service: X-Ray's default namespace")
	assert.Nil(t, metadata(t, span, "AWS.reserved"))
}

func TestPutMetadataDefaultsToTheService(t *testing.T) {
	t.Setenv(env.ServiceName, "from-env")
	tracer, recorder := recordingTracer(t, WithService("payment"))

	_, err := tracer.Capture(t.Context(), "step", func(ctx context.Context) (struct{}, error) {
		PutMetadata(ctx, "inside", 1, "")
		return struct{}{}, nil
	})
	require.NoError(t, err)
	assert.InDelta(t, 1, metadata(t, spanNamed(t, recorder, "## step"), "payment")["inside"], 0,
		"the service of the tracer that captured the function")

	ctx, end := spanContext(t)
	PutMetadata(ctx, "outside", 2, "")
	assert.InDelta(t, 2, metadata(t, end(), "from-env")["outside"], 0, "else POWERTOOLS_SERVICE_NAME")
}

func TestPutMetadataValues(t *testing.T) {
	t.Parallel()
	ctx, end := spanContext(t)

	PutMetadata(ctx, "raw", jsontext.Value(`{"a":1}`), "ns")
	PutMetadata(ctx, "bytes json", []byte(`[1,2]`), "ns")
	PutMetadata(ctx, "bytes", []byte{0xff, 0x00}, "ns")
	PutMetadata(ctx, "error", errors.New("boom"), "ns")
	PutMetadata(ctx, "nil", nil, "ns")
	PutMetadata(ctx, "reader", strings.NewReader("stream"), "ns")
	PutMetadata(ctx, "channel", make(chan int), "ns")
	PutMetadata(ctx, "bad raw", jsontext.Value(`{`), "ns")

	assert.Equal(t, map[string]any{
		"raw":        map[string]any{"a": float64(1)},
		"bytes json": []any{float64(1), float64(2)},
		"bytes":      "/wA=",
		"error":      "boom",
		"nil":        nil,
	}, metadata(t, end(), "ns"))
}

func TestPutMetadataKeepsASpanWithinBudget(t *testing.T) {
	t.Parallel()
	ctx, end := spanContext(t)

	big := strings.Repeat("x", metadataBudget/2)
	PutMetadata(ctx, "first", big, "a")
	PutMetadata(ctx, "second", big, "b")
	PutMetadata(ctx, "small", "fits", "b")

	span := end()
	assert.Contains(t, metadata(t, span, "a"), "first")
	assert.Equal(t, map[string]any{"small": "fits"}, metadata(t, span, "b"),
		"what would take the span past the budget is dropped, the rest kept")
}

// foreignSpan is a recording span of another OpenTelemetry implementation,
// whose attributes cannot be read back.
type foreignSpan struct {
	trace.Span

	mu    sync.Mutex
	attrs []attribute.KeyValue
}

func (s *foreignSpan) IsRecording() bool { return true }

func (s *foreignSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, kv...)
}

func TestPutOnASpanThatCannotBeRead(t *testing.T) {
	t.Parallel()
	span := &foreignSpan{Span: trace.SpanFromContext(t.Context())}
	ctx := trace.ContextWithSpan(t.Context(), span)

	PutAnnotation(ctx, "a", 1)
	PutAnnotation(ctx, "b", 2)
	PutMetadata(ctx, "k", "v", "ns")

	last := map[attribute.Key]attribute.Value{}
	for _, kv := range span.attrs {
		last[kv.Key] = kv.Value
	}
	assert.Equal(t, []string{"b"}, last[AnnotationsAttribute].AsStringSlice(), "only the last annotation is listed")
	assert.JSONEq(t, `{"k":"v"}`, last[MetadataAttributePrefix+"ns"].AsString())
}

func TestSanitizeAnnotationKey(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"ColdStart":       "ColdStart",
		"http.route":      "http_route",
		"a-b c":           "a_b_c",
		"under_score":     "under_score",
		"ünïcode":         "_n_code",
		"emoji\U0001F600": "emoji_",
	}
	for in, want := range tests {
		assert.Equal(t, want, SanitizeAnnotationKey(in), in)
	}
}
