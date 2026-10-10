// Package tracing is the Tracer of Powertools for AWS Lambda: an opinionated
// layer over OpenTelemetry that traces a Lambda function to AWS X-Ray, the way
// the AWS X-Ray SDK migration guide prescribes now that the X-Ray SDKs are in
// maintenance mode.
//
// # Getting started
//
// Build one Tracer per process and decorate the handler:
//
//	var tracer = tracing.New(tracing.WithService("payment"))
//
//	func main() {
//		lambda.Start(tracer.CaptureLambdaHandler(handle))
//	}
//
// The function needs active tracing (SAM: Tracing: Active), which also gives
// its role the xray:PutTraceSegments and xray:PutTelemetryRecords
// permissions.
//
// New builds the tracer provider of the guide's "Lambda manual
// instrumentation": the X-Ray UDP exporter writing OTLP spans to the endpoint
// Lambda runs next to the function (AWS_XRAY_DAEMON_ADDRESS), a simple span
// processor (Lambda freezes the environment between invocations, so nothing
// may wait in a batch), X-Ray trace ids, and the Lambda resource with
// service.name set to the service. It registers that provider and the X-Ray
// propagator as the OpenTelemetry globals (WithGlobals), so that every
// instrumentation in the process joins the same traces.
//
// # The invocation
//
// CaptureLambdaHandler wraps the handler with otellambda: every invocation is
// a server span, an X-Ray segment, whose parent is the trace Lambda started.
// The decorator adds what Powertools Python adds:
//
//   - the ColdStart annotation, true for the first invocation of the
//     execution environment (never under provisioned concurrency);
//   - the Service annotation, when the tracer has a service;
//   - the handler's result as metadata "<handler> response", in the
//     service's namespace;
//   - a returned error or a panic: recorded on the span with the failed
//     status, and as metadata "<handler> error". A panic is resumed after
//     being recorded.
//
// The spans of the invocation are flushed when it ends.
//
// # Functions
//
// CaptureMethod decorates any func(context.Context, In) (Out, error) so that
// each call is a span named "## <package>.<function>", an X-Ray subsegment,
// with the same response and error capture. Capture does the same for a
// block of code:
//
//	total, err := tracer.Capture(ctx, "charge", func(ctx context.Context) (int, error) {
//		return charge(ctx, order)
//	})
//
// WithCaptureResponse and WithCaptureError turn the capture off for one
// function; the POWERTOOLS_TRACER_CAPTURE_RESPONSE and
// POWERTOOLS_TRACER_CAPTURE_ERROR variables do it for all of them. Turn the
// response capture off for results that are sensitive or large.
//
// # Annotations and metadata
//
// PutAnnotation adds a string, number or boolean to the current span as an
// X-Ray annotation, indexed for filter expressions and trace groups. X-Ray
// turns an attribute into an annotation when its key is listed in the
// aws.xray.annotations attribute; PutAnnotation keeps that list. Annotation
// keys hold letters, digits and underscores: other characters become
// underscores.
//
// PutMetadata adds any JSON-encodable value to the current span as X-Ray
// metadata, not indexed, under a namespace (by default the service). X-Ray
// turns plain attributes into metadata of the "default" namespace; to keep
// Powertools' namespaces, the entries of a namespace are one attribute,
// "aws.xray.metadata.<namespace>", holding their JSON object, which is how the
// OpenTelemetry X-Ray exporter spells a metadata namespace.
//
// Both act on the span in the context: the invocation inside a captured
// handler, the function's span inside a captured function. Without a
// recording span (tracing disabled) they do nothing.
//
// # AWS SDK and HTTP calls
//
// Powertools Python patches the libraries the X-Ray SDK supports. In Go, a
// client is instrumented when it is built: AWSTracerProvider is the tracer
// provider of the AWS SDK for Go v2 clients (the SDK's own smithyoteltracing
// adapter, which replaced the deprecated otelaws), and Transport and
// HTTPClient trace net/http requests with otelhttp, carrying the X-Ray trace
// header downstream. IgnoreEndpoint leaves some requests untraced.
//
// # Disabling tracing
//
// Tracing is off, and every decorator returns the function it is given, when
// POWERTOOLS_TRACE_DISABLED is true, outside Lambda (no LAMBDA_TASK_ROOT),
// under the AWS SAM CLI (AWS_SAM_LOCAL), or when Lambda gives the function no
// X-Ray endpoint (no AWS_XRAY_DAEMON_ADDRESS: active tracing is off).
// WithDisabled decides regardless of the environment. Tests therefore need no
// setup; to look at the spans, pass a recording provider with
// WithTracerProvider (go.opentelemetry.io/otel/sdk/trace/tracetest).
//
// # Environment variables
//
//	POWERTOOLS_SERVICE_NAME             the service (WithService)
//	POWERTOOLS_TRACE_DISABLED           turns tracing off (WithDisabled)
//	POWERTOOLS_TRACER_CAPTURE_RESPONSE  response capture, default true
//	POWERTOOLS_TRACER_CAPTURE_ERROR     error capture, default true
//	AWS_XRAY_DAEMON_ADDRESS             the X-Ray endpoint, set by Lambda
//	LAMBDA_TASK_ROOT, AWS_SAM_LOCAL     where the function runs
//
// # Concurrency
//
// Lambda Managed Instances run several invocations at once in one process.
// The tracer keeps nothing per invocation outside the context: the parent of
// each invocation span comes from the trace header aws-lambda-go puts in the
// invocation's context (it sets _X_AMZN_TRACE_ID only when the environment
// runs one invocation at a time), the cold start is claimed by exactly one
// invocation, and annotations of one span made from several goroutines are
// serialized.
//
// # Differences from Powertools Python
//
//   - The X-Ray SDK is replaced by OpenTelemetry. The span otellambda opens
//     for the invocation carries what Python's "## <handler>" subsegment
//     carries; captured functions keep the "## <name>" spans.
//   - Tracer is a value built by New, not a class whose configuration every
//     instance shares; share the value, or use the OpenTelemetry globals New
//     registers. Provider is the escape hatch (Python's tracer.provider).
//   - patch, patch_modules and auto_patch become explicit instrumentation:
//     AWSTracerProvider, Transport, HTTPClient. aiohttp_trace_config is
//     Python only.
//   - Generators, context managers and async functions have no Go form:
//     capture the code that consumes an iterator with Capture, and goroutines
//     need no special care (OpenTelemetry contexts are goroutine safe).
//   - Values are encoded with encoding/json/v2 instead of jsonpickle; an
//     error is stored as its message, and a stream (io.Reader) is never
//     consumed. Metadata that would take a span past 32 KiB is dropped,
//     where Python lets the X-Ray daemon reject the segment ("message too
//     long").
//   - The tracer turns itself off when there is no X-Ray endpoint, which
//     Python cannot see. AWS_CHALICE_CLI_MODE is Python only.
//   - The cold start is one decision shared by every Powertools utility.
package tracing
