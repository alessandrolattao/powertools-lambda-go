package tracing

import (
	"context"
	"reflect"
	"runtime"
	"strings"

	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// lambdaTraceHeaderKey is the context key aws-lambda-go keeps the X-Ray trace
// header of each invocation under, a plain string.
const lambdaTraceHeaderKey = "x-amzn-trace-id"

// withLambdaParent makes the trace Lambda started for this invocation the
// remote parent in ctx. The header comes from the context, where
// aws-lambda-go puts it for every invocation; _X_AMZN_TRACE_ID is only the
// last resort, since aws-lambda-go sets it only while the environment runs
// one invocation at a time and Lambda Managed Instances run several. A span
// already active in ctx stays the parent.
func withLambdaParent(ctx context.Context) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	header, ok := ctx.Value(lambdaTraceHeaderKey).(string)
	if !ok || header == "" {
		header = env.Get(env.XRayTraceID)
	}
	if header == "" {
		return ctx
	}
	carrier := propagation.HeaderCarrier{}
	carrier.Set("X-Amzn-Trace-Id", header)
	return xray.Propagator{}.Extract(ctx, carrier)
}

// SanitizeSpanName removes from name the characters X-Ray does not accept in
// a segment name (?;*()!$~^<>), as Powertools Python does with the names of
// captured methods.
func SanitizeSpanName(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("?;*()!$~^<>", r) {
			return -1
		}
		return r
	}, name)
}

// methodName names a captured function the way Powertools Python names a
// captured method, module and qualified name: "orders.charge",
// "orders.Service.Charge" for a method value, "main.main.func1" for a closure.
func methodName(fn any) string {
	pc := reflect.ValueOf(fn).Pointer()
	f := runtime.FuncForPC(pc)
	if f == nil {
		return "function"
	}
	name := f.Name()
	// The package path up to its last element: "github.com/acme/orders.charge".
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	name = strings.TrimSuffix(name, "-fm") // a method value
	name = strings.ReplaceAll(name, "[...]", "")
	return SanitizeSpanName(name)
}

// handlerName names a handler the way Powertools Python does, by its bare
// function name: "handle" for main.handle.
func handlerName(fn any) string {
	name := methodName(fn)
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = name[dot+1:]
	}
	return name
}
