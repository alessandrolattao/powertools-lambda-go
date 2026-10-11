package logging

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// traceHeaderKey is the context key under which aws-lambda-go stores the
// invocation's X-Amzn-Trace-Id header. It is a plain string because that is
// what aws-lambda-go uses.
const traceHeaderKey = "x-amzn-trace-id"

// xrayTraceID returns the X-Ray trace id of the invocation ctx belongs to:
// the active OpenTelemetry span's, written the X-Ray way; else the Root of
// the trace header aws-lambda-go puts in the context; else the Root of
// _X_AMZN_TRACE_ID, which aws-lambda-go sets only when it runs one
// invocation at a time, and Powertools Python reads unconditionally.
func xrayTraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		id := sc.TraceID().String()
		return "1-" + id[:8] + "-" + id[8:]
	}
	if header, ok := ctx.Value(traceHeaderKey).(string); ok {
		if root := traceRoot(header); root != "" {
			return root
		}
	}
	return traceRoot(env.Get(env.XRayTraceID))
}

// traceRoot extracts the trace id from an X-Amzn-Trace-Id header, such as
// "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1".
// Python takes whatever precedes the first semicolon; the Root field is
// looked for wherever it is, and a header without one gives "".
func traceRoot(header string) string {
	for field := range strings.SplitSeq(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Root") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
