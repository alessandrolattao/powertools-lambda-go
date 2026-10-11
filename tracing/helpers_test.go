package tracing

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The X-Ray trace header of the tests, and the trace and parent ids it
// carries in OpenTelemetry form.
const (
	testHeader = "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1;Lineage=a87bd80c:1"
	testTrace  = "5759e988bd862e3fe1be46a994272793"
	testParent = "53995c3f42cd8ad8"
)

// recordingTracer returns a Tracer whose spans are kept in memory, without
// touching the OpenTelemetry globals.
func recordingTracer(t *testing.T, opts ...Option) (*Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, tp.Shutdown(context.Background())) })
	opts = append([]Option{WithTracerProvider(tp), WithGlobals(false)}, opts...)
	return New(opts...), recorder
}

// invocation returns the context aws-lambda-go hands a handler: the Lambda
// context and, when header is not empty, the X-Ray header under its string key.
func invocationContext(t *testing.T, requestID, header string) context.Context {
	t.Helper()
	ctx := lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{
		AwsRequestID:       requestID,
		InvokedFunctionArn: "arn:aws:lambda:eu-west-1:123456789012:function:payment",
	})
	if header != "" {
		ctx = context.WithValue(ctx, lambdaTraceHeaderKey, header) //nolint:revive,staticcheck // aws-lambda-go stores the header under this very string key
	}
	return ctx
}

// spanNamed returns the ended span called name.
func spanNamed(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range recorder.Ended() {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("no span named %q among %d", name, len(recorder.Ended()))
	return nil
}

// attributes indexes the attributes of span by key.
func attributes(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	return attrs
}

// metadata decodes the metadata of span in namespace; nil when there is none.
func metadata(t *testing.T, span sdktrace.ReadOnlySpan, namespace string) map[string]any {
	t.Helper()
	value, ok := attributes(span)[attribute.Key(MetadataAttributePrefix+namespace)]
	if !ok {
		return nil
	}
	var entries map[string]any
	require.NoError(t, json.Unmarshal([]byte(value.AsString()), &entries))
	return entries
}

// annotationKeys returns the keys span lists as X-Ray annotations.
func annotationKeys(span sdktrace.ReadOnlySpan) []string {
	return attributes(span)[AnnotationsAttribute].AsStringSlice()
}
