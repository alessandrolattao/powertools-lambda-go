# Tracer

Tracer is an opinionated layer over [OpenTelemetry](https://opentelemetry.io/)
that traces a Lambda function to AWS X-Ray. The X-Ray SDKs entered maintenance
mode on 2026-02-25, and the
[AWS X-Ray migration guide](https://docs.aws.amazon.com/xray/latest/devguide/xray-sdk-migration.html)
prescribes OpenTelemetry with the X-Ray UDP exporter for Lambda: that is what
`tracing` builds for you.

## Key features

* Auto capture cold start and service as annotations, and responses or full
  errors as metadata
* Auto-disable when not running in AWS Lambda, or without active tracing
* Annotations and metadata, with Powertools' metadata namespaces, on
  OpenTelemetry spans
* AWS SDK for Go v2 and `net/http` instrumentation, the Go form of patching
* Correct traces under Lambda Managed Instances, where invocations run
  concurrently in one process

## Getting started

### Install

```sh
go get github.com/alessandrolattao/powertools-lambda-go
```

### Permissions

The function needs active tracing, which also grants its role the
`xray:PutTraceSegments` and `xray:PutTelemetryRecords` permissions:

```yaml
Resources:
  PaymentFunction:
    Type: AWS::Serverless::Function
    Properties:
      Runtime: provided.al2023
      Handler: bootstrap
      Tracing: Active
      Environment:
        Variables:
          POWERTOOLS_SERVICE_NAME: payment
```

With active tracing, Lambda sets `AWS_XRAY_DAEMON_ADDRESS`, the X-Ray endpoint
the tracer sends spans to. Without it, the tracer turns itself off.

### Lambda handler

Build one `Tracer` per process and decorate the handler with
`CaptureLambdaHandler`:

```go
var tracer = tracing.New(tracing.WithService("payment"))

func handle(ctx context.Context, o Order) (Receipt, error) {
	tracing.PutAnnotation(ctx, "order_id", o.ID)
	return Receipt{OrderID: o.ID, Paid: true}, nil
}

func main() {
	lambda.Start(tracer.CaptureLambdaHandler(handle))
}
```

`New` builds the tracer provider of the guide's "Lambda manual
instrumentation": the X-Ray UDP exporter writing to `AWS_XRAY_DAEMON_ADDRESS`
through a simple span processor (Lambda freezes the environment between
invocations, so spans are sent as they end), X-Ray trace ids, the X-Ray
propagator, and the Lambda resource with `service.name` set to your service.

`CaptureLambdaHandler` wraps the handler with
[otellambda](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-lambda-go/otellambda):
each invocation is a server span, an X-Ray segment, joined to the trace Lambda
started. On it, the decorator:

* creates a `ColdStart` annotation, to filter traces that paid for the
  initialization of the execution environment;
* creates a `Service` annotation, when `WithService` or
  `POWERTOOLS_SERVICE_NAME` sets a service;
* captures the response, or the error, as metadata;
* records a panic, with its stack trace, before letting it continue.

It flushes the spans of the invocation before returning.

### Annotations & Metadata

**Annotations** are key-values indexed by X-Ray: filter expressions and trace
groups select traces by them.

```go
_, err := tracer.Capture(ctx, "confirm", func(ctx context.Context) (struct{}, error) {
	tracing.PutAnnotation(ctx, "order.id", "o-1") // stored as order_id
	tracing.PutAnnotation(ctx, "amount", 5)
	tracing.PutAnnotation(ctx, "express", true)
	return struct{}{}, nil
})
```

Values are strings, numbers and booleans. X-Ray accepts letters, digits and
underscores in a key; other characters become underscores. X-Ray indexes up to
50 annotations per trace.

**Metadata** are key-values not indexed, for detail such as payloads. Any
value that encodes to JSON goes, under a namespace that defaults to the
service:

```go
total, err := tracer.Capture(ctx, "sum", func(ctx context.Context) (int, error) {
	tracing.PutMetadata(ctx, "lines", []int{1, 2, 3}, "cart")
	return 6, nil
})
```

Both act on the span in the context: the invocation inside the handler, the
function's span inside a captured function. Without a span to act on (tracing
off), they do nothing.

### Functions

`CaptureMethod` decorates any `func(context.Context, In) (Out, error)`: each
call is a span named `## <package>.<function>`, an X-Ray subsegment, with the
same response and error capture as the handler (no cold start annotation).

```go
charge := tracer.CaptureMethod(func(_ context.Context, o Order) (int, error) {
	return o.Amount * 100, nil
}, tracing.WithName("charge"))

cents, err := charge(ctx, Order{ID: "o-1", Amount: 5}) // span "## charge"
```

`Capture` does the same for a block of code, a closure, or a durable step:

```go
got, err := tracer.Capture(ctx, "fetch", func(ctx context.Context) (string, error) {
	return fetch(ctx, key)
})
```

The function receives the context that carries its span: calls made with it
nest under the span.

???+ note
    Function responses are captured as JSON, encoded with `encoding/json/v2`.
    A `jsontext.Value` or a `[]byte` holding JSON is kept as that document, an
    error as its message, and a stream (`io.Reader`) is never read. A nil
    result and `struct{}` are not captured.

### Environment variables

| Setting | Description | Environment variable | Default |
|---|---|---|---|
| **Service** | The service of the traces | `POWERTOOLS_SERVICE_NAME` | none |
| **Disable Tracing** | Explicitly disables all tracing | `POWERTOOLS_TRACE_DISABLED` | `false` |
| **Response Capture** | Captures Lambda or function return as metadata | `POWERTOOLS_TRACER_CAPTURE_RESPONSE` | `true` |
| **Error Capture** | Captures Lambda or function error as metadata | `POWERTOOLS_TRACER_CAPTURE_ERROR` | `true` |

Both capture variables can be overridden per function with
`WithCaptureResponse` and `WithCaptureError`. Lambda sets
`AWS_XRAY_DAEMON_ADDRESS`, `LAMBDA_TASK_ROOT` and, under the SAM CLI,
`AWS_SAM_LOCAL`.

## Advanced

### How spans become X-Ray data

X-Ray converts OpenTelemetry spans: a server span becomes a segment, any other
span a subsegment, and attributes become metadata of the `default` namespace.

* An attribute becomes an **annotation** when its key is listed in the
  `aws.xray.annotations` attribute. `PutAnnotation` sets the attribute and
  keeps that list, even when several goroutines annotate the same span.
* Powertools' **metadata namespaces** have no OpenTelemetry form. The entries
  of a namespace are kept in one attribute, `aws.xray.metadata.<namespace>`,
  holding their JSON object: the spelling the OpenTelemetry X-Ray exporter
  turns into the metadata of that namespace. Other backends show the
  attribute as a JSON string.
* A span reaches the X-Ray endpoint as one UDP datagram. Metadata that would
  take a span past 32 KiB of JSON is dropped, so that the span, and its
  annotations, still arrive.

Python opens a `## <handler>` subsegment for the handler. In Go the invocation
span otellambda opens is that span: annotations and metadata of the handler go
there.

### Disabling response auto-capture

Use `WithCaptureResponse(false)` on `CaptureLambdaHandler`, `CaptureMethod` or
`Capture` not to add a result as metadata. This is useful when the result is
sensitive, too large for a span, or a stream (streams are never read anyway):

```go
total, err := tracer.Capture(ctx, "sum", func(ctx context.Context) (int, error) {
	tracing.PutMetadata(ctx, "lines", []int{1, 2, 3}, "cart")
	return 6, nil
}, tracing.WithCaptureResponse(false))
```

### Disabling error auto-capture

Use `WithCaptureError(false)` not to add the error as metadata, when error
messages may hold data you do not control. The span still records the error
and its failed status, as X-Ray records the exception of a Python subsegment
whatever `capture_error` says.

### Tracing AWS SDK calls

Powertools Python patches botocore. In Go, AWS SDK for Go v2 clients trace
their calls through the SDK's own OpenTelemetry adapter, `smithyoteltracing`
(the `otelaws` instrumentation is deprecated). `AWSTracerProvider` returns it,
bound to the tracer's provider:

```go
db := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
	o.TracerProvider = tracer.AWSTracerProvider()
})
```

Each call becomes a client span, named like `DynamoDB.ListTables`, under the
span of the context it is made with.

### Tracing HTTP calls

`HTTPClient` and `Transport` trace `net/http` requests with
[otelhttp](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp)
and send the X-Ray trace header downstream. Requests must carry the
invocation's context:

```go
client := tracer.HTTPClient()
req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
resp, err := client.Do(req)
```

### Ignoring certain HTTP endpoints

`IgnoreEndpoint` leaves requests untraced by hostname (a glob) and paths, as
Python's `ignore_endpoint` does:

```go
client := tracer.HTTPClient(
	tracing.IgnoreEndpoint("", "/health"),
	tracing.IgnoreEndpoint("*.example.com"),
)
```

### Escape hatch

`Provider` returns the OpenTelemetry tracer provider, for anything OpenTelemetry
offers that the tracer does not wrap. `New` also registers it, and the X-Ray
propagator, as the OpenTelemetry globals, so other instrumentation joins the
same traces; `WithGlobals(false)` leaves the globals alone.

`WithTracerProvider` makes the tracer use a provider you built: it is not
turned off outside Lambda, and `Shutdown` leaves it alone.

### Reusing Tracer across your code

A `Tracer` is a value, safe for concurrent use: build it once in `main` (or at
package level) and pass it where functions are captured. `PutAnnotation` and
`PutMetadata` need no tracer, only the context.

### Lambda Managed Instances

Managed Instances run several invocations at once in one Go process.
aws-lambda-go then leaves `_X_AMZN_TRACE_ID` unset, so the tracer takes each
invocation's trace header from its context, where aws-lambda-go always puts
it: concurrent invocations join their own traces. The cold start is claimed by
exactly one invocation. Sending traces needs
[VPC connectivity](lambda-features.md#vpc-connectivity) to X-Ray.

### Durable functions

Trace the invocation outside the durable execution, and capture work inside
step bodies. See [Lambda features](lambda-features.md#durable-functions).

## Testing your code

Tracer is off outside Lambda, so tests need no setup: decorators return the
functions they get and nothing is exported.

To look at the spans, give the tracer a provider that records them in memory:

```go
recorder := tracetest.NewSpanRecorder()
tracer := tracing.New(
	tracing.WithService("payment"),
	tracing.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))),
	tracing.WithGlobals(false),
)

handler := tracer.CaptureLambdaHandler(handle)
ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{AwsRequestID: "req-1"})
_, err := handler(ctx, Order{ID: "o-1", Amount: 5})

span := recorder.Ended()[0]
```

The package's own tests go one step further: a UDP listener plays the X-Ray
endpoint, decodes what the exporter sends (the X-Ray daemon header, the `T1S`
prefix, a base64 OTLP request) and checks the annotations, the metadata, the
error status and the parent from the Lambda trace header.

## Differences from Powertools Python

| Python | Go |
|---|---|
| X-Ray SDK | OpenTelemetry, exported with the X-Ray UDP exporter |
| `Tracer()` instances share their configuration | `tracing.New` returns a value to share |
| `## handler` subsegment | the invocation span otellambda opens |
| `capture_method` on functions, generators, context managers, coroutines | `CaptureMethod` and `Capture` |
| `patch`, `patch_modules`, `auto_patch` | `AWSTracerProvider`, `Transport`, `HTTPClient` |
| `ignore_endpoint` | `IgnoreEndpoint` |
| `aiohttp_trace_config` | not ported (Python only) |
| `tracer.provider` | `Provider` |
| `jsonpickle` serialization | `encoding/json/v2`; errors as messages, streams skipped, 32 KiB per span |
| disabled outside Lambda, under SAM and Chalice | disabled outside Lambda, under SAM, and without an X-Ray endpoint |
| cold start flag per utility | one cold start decision shared by every utility |
