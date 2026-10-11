package integration

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/tracing"
)

// recordingTracer returns a tracer whose spans stay in memory.
func recordingTracer(t *testing.T) (tracer *tracing.Tracer, recorder *tracetest.SpanRecorder) {
	t.Helper()
	recorder = tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { assert.NoError(t, tp.Shutdown(context.Background())) })
	return tracing.New(
		tracing.WithService("integration"),
		tracing.WithTracerProvider(tp),
		tracing.WithGlobals(false),
	), recorder
}

// ancestors returns the names of the spans above span, nearest first.
func ancestors(spans []sdktrace.ReadOnlySpan, span sdktrace.ReadOnlySpan) []string {
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byID[s.SpanContext().SpanID()] = s
	}
	var names []string
	for parent, ok := byID[span.Parent().SpanID()]; ok; parent, ok = byID[parent.Parent().SpanID()] {
		names = append(names, parent.Name())
	}
	return names
}

func TestTracingAWSCallsNestUnderTheInvocation(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	db := dynamodb.NewFromConfig(suite.aws, func(o *dynamodb.Options) {
		o.TracerProvider = tracer.AWSTracerProvider()
	})
	identity := sts.NewFromConfig(suite.aws, func(o *sts.Options) {
		o.TracerProvider = tracer.AWSTracerProvider()
	})

	listTables := tracer.CaptureMethod(func(ctx context.Context, limit int32) (int, error) {
		out, err := db.ListTables(ctx, &dynamodb.ListTablesInput{Limit: &limit})
		if err != nil {
			return 0, err
		}
		return len(out.TableNames), nil
	}, tracing.WithName("listTables"))

	handler := tracer.CaptureLambdaHandler(func(ctx context.Context, _ struct{}) (string, error) {
		if _, err := listTables(ctx, 10); err != nil {
			return "", err
		}
		out, err := identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return "", err
		}
		return *out.Account, nil
	})

	ctx := lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{AwsRequestID: "integration-1"})
	account, err := handler(ctx, struct{}{})
	require.NoError(t, err)
	require.NotEmpty(t, account)

	spans := recorder.Ended()
	var listTablesCall, callerIdentityCall sdktrace.ReadOnlySpan
	for _, span := range spans {
		switch span.Name() {
		case "DynamoDB.ListTables":
			listTablesCall = span
		case "STS.GetCallerIdentity":
			callerIdentityCall = span
		}
	}
	require.NotNil(t, listTablesCall, "the DynamoDB call is a span")
	require.NotNil(t, callerIdentityCall, "the STS call is a span")
	assert.Equal(t, trace.SpanKindClient, listTablesCall.SpanKind())

	assert.Equal(t, []string{"## listTables", ""}, ancestors(spans, listTablesCall),
		"the call nests in the captured method, which nests in the invocation")
	assert.Equal(t, []string{""}, ancestors(spans, callerIdentityCall))
}
