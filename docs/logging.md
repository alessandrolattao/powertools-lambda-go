# Logger

The logger writes structured JSON records for Lambda, with the keys Powertools for AWS Lambda (Python) writes, so that
a Go and a Python function share log queries, dashboards and alarms.

It is built on `log/slog`: the `logging.Handler` is a `slog.Handler`, and the `logging.Logger` is a `*slog.Logger`
with the Powertools helpers next to slog's methods.

## Key features

* Captures key fields from the Lambda context, the cold start, and structures every record as JSON
* Logs the incoming event when instructed (disabled by default)
* Logs at DEBUG a percentage of the invocations (sampling, disabled by default)
* Appends keys to the records of an invocation, or of a part of it
* Buffers the low-level records of an invocation and writes them only when something goes wrong
* Keeps every invocation apart when Lambda Managed Instances run several of them at once in one process

## Getting started

The logger needs two settings:

| Setting | Description | Environment variable | Option |
|---|---|---|---|
| **Logging level** | How verbose the logger is (INFO by default) | `POWERTOOLS_LOG_LEVEL` | `logging.WithLevel` |
| **Service** | The `service` key of every record | `POWERTOOLS_SERVICE_NAME` | `logging.WithService` |

```yaml
Resources:
  PaymentFunction:
    Type: AWS::Serverless::Function
    Properties:
      Runtime: provided.al2023
      Handler: bootstrap
      Environment:
        Variables:
          POWERTOOLS_SERVICE_NAME: payment
          POWERTOOLS_LOG_LEVEL: INFO
```

Create the logger once, at package level or in `main`, and log with slog's `Context` methods: the context is where a
record finds its invocation.

```go
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
)

var logger = logging.New(logging.WithService("payment"))

type Payment struct {
	ChargeID string `json:"charge_id"`
}

func handle(ctx context.Context, payment Payment) (string, error) {
	logger.InfoContext(ctx, "collecting payment", "charge_id", payment.ChargeID)
	return "collected", nil
}

func main() {
	lambda.Start(logger.InjectContext(handle))
}
```

### Standard structured keys

| Key | Example | Note |
|---|---|---|
| **level** | `INFO` | Lambda's level names: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`, `FATAL` |
| **location** | `main.handle:21` | Function and line the record was logged at |
| **message** | `collecting payment` | |
| **timestamp** | `2026-10-10T12:00:00.123Z` | RFC 3339 with milliseconds, in UTC |
| **service** | `payment` | `service_undefined` when not set |
| **xray_trace_id** | `1-5759e988-bd862e3fe1be46a994272793` | When tracing is enabled |
| **sampling_rate** | `0.1` | When sampling is enabled |
| **exception_name** | `PathError` | When an error is logged under the `err` key |
| **exception** | `open /x: no such file or directory` | When an error is logged under the `err` key |

The level names and the timestamp format are the ones Lambda's log-level filtering reads (Advanced Logging Controls):
a record with Python's `WARNING` or with Python's default timestamp is filed under INFO by Lambda. Python's format is an
[option](#date-formatting-and-level-names).

### Capturing Lambda context info

Every record logged with an invocation's context carries the Lambda context keys, whether or not the handler is wrapped
by `InjectContext`:

| Key | Example |
|---|---|
| **cold_start** | `false` |
| **function_name** | `payment` |
| **function_memory_size** | `128` |
| **function_arn** | `arn:aws:lambda:eu-west-1:123456789012:function:payment` |
| **function_request_id** | `899856cb-83d1-40d7-8611-9e78f15f32f4` |

```json
{"level":"INFO","location":"main.handle:21","message":"collecting payment","timestamp":"2026-10-10T12:00:00.123Z","service":"payment","cold_start":true,"function_name":"payment","function_memory_size":128,"function_arn":"arn:aws:lambda:eu-west-1:123456789012:function:payment","function_request_id":"52fdfc07-2182-154f-163f-5f0f9a621d72","charge_id":"ch_42","xray_trace_id":"1-5759e988-bd862e3fe1be46a994272793"}
```

The keys come from the context of each record, not from the logger, so that concurrent invocations (Lambda Managed
Instances) each log their own. A record logged without the context (`logger.Info` rather than `logger.InfoContext`) has
none of them.

`InjectContext` wraps the handler with the per-invocation work: the state the invocation's keys, correlation id,
sampling decision and log buffer live in, event logging, the correlation id, flushing the buffer on failure, logging
panics.

### Logging incoming event

When debugging in non-production environments, log the incoming event with `WithLogEvent` or the
`POWERTOOLS_LOGGER_LOG_EVENT` variable. It is off by default, since events often carry personal data. The event is the
record's message, as a JSON value.

```go
handle := logger.InjectContext(func(context.Context, order) (string, error) {
	return "accepted", nil
}, logging.WithLogEvent(true))
```

```json
{"level":"INFO","message":{"id":"o-1"},"timestamp":"2026-10-10T12:00:00.123Z","service":"orders"}
```

### Setting a correlation ID

`WithCorrelationIDPath` selects the correlation id of every invocation in its event with a JMESPath expression,
the Powertools functions of the `jmespath` package included:

```go
wrapped := logger.InjectContext(handle, logging.WithCorrelationIDPath(logging.CorrelationAPIGatewayHTTP))
```

```json
{"level":"INFO","message":"listing orders","timestamp":"2026-10-10T12:00:00.123Z","service":"orders","correlation_id":"r-1","route":"GET /orders"}
```

An expression that does not compile, or fails on an event, fails the invocation, as in Powertools Python.

#### SetCorrelationID

Set it anywhere else in the invocation, and read it back:

```go
logging.SetCorrelationID(ctx, event.ID) // an EventBridge event's id
id := logging.CorrelationID(ctx)
```

An empty id removes it.

#### Known correlation IDs

| Constant | Expression | Event |
|---|---|---|
| `CorrelationAPIGatewayREST` | `requestContext.requestId` | API Gateway REST API |
| `CorrelationAPIGatewayHTTP` | `requestContext.requestId` | API Gateway HTTP API |
| `CorrelationLambdaFunctionURL` | `requestContext.requestId` | Lambda function URL |
| `CorrelationAppSyncAuthorizer` | `requestContext.requestId` | AppSync authorizer |
| `CorrelationAppSyncResolver` | `request.headers."x-amzn-trace-id"` | AppSync resolver |
| `CorrelationApplicationLoadBalancer` | `headers."x-amzn-trace-id"` | Application Load Balancer |
| `CorrelationVPCLattice` | `headers."x-amzn-trace-id"` | VPC Lattice |
| `CorrelationEventBridge` | `id` | EventBridge |
| `CorrelationS3ObjectLambda` | `xAmzRequestId` | S3 Object Lambda |

### Appending additional keys

* Keys of every record of a logger: slog's `With`, `logger.With("tenant", "acme")`.
* Keys of one record: the record's own attributes, `logger.InfoContext(ctx, "charged", "amount", 42)`.
* Keys of the rest of an invocation: `logging.AppendKeys(ctx, "order_id", id)`.
* Keys of a part of an invocation: `ctx = logging.WithKeys(ctx, "step", "charge")`.

Keys behave as in a Python dict: a key appears once, a later value replaces an earlier one in its place, and a `nil`
value removes the key. The record's attributes win over the logger's, which win over the invocation's, which win over
the Powertools keys. `level`, `location`, `message` and `timestamp` cannot be overridden.

#### AppendKeys

```go
func handle(ctx context.Context, event Order) (string, error) {
	logging.AppendKeys(ctx, "order_id", event.ID)
	logger.InfoContext(ctx, "collecting payment")
	return "collected", nil
}
```

Unlike Python's `append_keys`, the keys end with the invocation: Lambda Managed Instances run invocations side by side,
and the next invocation never inherits the previous one's keys. `AppendKeys` needs the state `InjectContext` puts in
the context; outside it, it does nothing.

#### WithKeys

`WithKeys` returns a context whose records carry the keys, Python's `append_context_keys`:

```go
ctx := logging.WithKeys(context.Background(), "step", "charge")
logger.InfoContext(ctx, "charging")
logger.InfoContext(context.Background(), "done")
```

```json
{"level":"INFO","message":"charging","timestamp":"2026-10-10T12:00:00.123Z","service":"orders","step":"charge"}
{"level":"INFO","message":"done","timestamp":"2026-10-10T12:00:00.123Z","service":"orders"}
```

The keys end with the context, and the values they shadowed come back: unlike Python, leaving the scope never loses a
key that existed before it.

### Removing additional keys

```go
logging.RemoveKeys(ctx, "order_id")
```

`RemoveKeys` removes keys added by `AppendKeys`; the keys of `WithKeys` stay with their context.

#### Clearing all state

`logging.ClearState(ctx)` removes the keys of `AppendKeys` and the correlation id. The keys of an invocation never reach
the next one, so `InjectContext` needs no `clear_state`; `WithClearState(true)` starts the invocation without the keys
a middleware wrapping it put in the context.

### Accessing currently configured keys

`logging.CurrentKeys(ctx)` returns the keys the context adds to records: the correlation id, the keys of `AppendKeys`,
then those of `WithKeys`, as a copy.

### Log levels

| Level | Name | Python |
|---|---|---|
| `logging.LevelTrace` | `TRACE` | |
| `slog.LevelDebug` | `DEBUG` | `DEBUG` |
| `slog.LevelInfo` | `INFO` | `INFO` |
| `slog.LevelWarn` | `WARN` | `WARNING` |
| `slog.LevelError` | `ERROR` | `ERROR` |
| `logging.LevelFatal` (`LevelCritical`) | `FATAL` | `CRITICAL` |

Set it with `logging.WithLevel`, `logger.SetLevel`, or the variables; `logging.ParseLevel` reads Lambda's and Python's
names. `logger.Level()` returns it.

#### AWS Lambda Advanced Logging Controls (ALC)

With Advanced Logging Controls, Lambda sets `AWS_LAMBDA_LOG_LEVEL` and drops the records below it. The logger follows
Powertools Python's precedence:

1. `AWS_LAMBDA_LOG_LEVEL`
2. `logging.WithLevel`, or `logger.SetLevel`
3. `POWERTOOLS_LOG_LEVEL`, then the legacy `LOG_LEVEL`
4. INFO

When the configured level is more verbose than Lambda's, the logger warns that records will be lost.

### Logging exceptions

An error under the `err` key is written as `exception` (its message) and `exception_name` (the name of its type, the
first type in the wrapped chain that is not the standard library's own wrapper):

```go
logger.ErrorContext(ctx, "charge failed", "err", err)
```

```json
{"level":"ERROR","location":"main.handle:30","message":"charge failed","timestamp":"2026-10-10T12:00:00.123Z","service":"payment","exception":"charge: open /x: file does not exist","exception_name":"PathError"}
```

A plain `errors.New` error is named `error`. Keys `exception` or `exception_name` set by the record win.

#### Uncaught panics

`WithLogUncaughtPanics(true)` makes `InjectContext` log a panicking handler at ERROR, located where it panicked, with
the stack, before the panic goes on to aws-lambda-go. Errors the handler returns are not logged: Lambda records them,
and Python does not log the exceptions that propagate either.

```go
var logger = logging.New(logging.WithLogUncaughtPanics(true))
```

#### Stack trace logging

Panics, and errors that carry their stack (a `Callers() []uintptr` method, the `logging.StackTracer` convention of
go-errors and similar packages), get a `stack_trace` key in Powertools Python's shape. `WithStackTrace(false)` turns it
off.

```json
"stack_trace":{"type":"plainError","value":"assignment to entry in nil map","module":"runtime","frames":[{"file":"/var/task/main.go","line":42,"function":"main.handle"}]}
```

### Date formatting and level names

The default timestamp is RFC 3339 with milliseconds in UTC, `2026-10-10T12:00:00.123Z`. `WithTimestampFormat` takes
any `time.Format` layout; `logging.TimestampPython` is Python's default, `2026-10-10 12:00:00,123+0000`.
`WithUTC(false)` writes local time, which the `TZ` variable sets. `WithLevelNames(logging.PythonLevelName)` writes
Python's level names.

```go
logger := logging.New(
	logging.WithLevelNames(logging.PythonLevelName),
	logging.WithTimestampFormat(logging.TimestampPython),
)
```

```json
{"level":"WARNING","message":"as Powertools Python writes it","timestamp":"2026-10-10 12:00:00,123+0000","service":"orders"}
```

Lambda's log-level filtering does not read Python's level names or timestamps: use them only when sharing log queries
with Python functions matters more.

### Environment variables

| Setting | Description | Environment variable | Default |
|---|---|---|---|
| **Service** | Sets the `service` key | `POWERTOOLS_SERVICE_NAME` | `service_undefined` |
| **Level** | Minimum level | `POWERTOOLS_LOG_LEVEL`, then `LOG_LEVEL` | `INFO` |
| **Lambda level** | Advanced Logging Controls, highest precedence | `AWS_LAMBDA_LOG_LEVEL` | |
| **Event Logging** | Whether to log the incoming event | `POWERTOOLS_LOGGER_LOG_EVENT` | `false` |
| **Debug Sample Rate** | Sets the debug log sampling | `POWERTOOLS_LOGGER_SAMPLE_RATE` | none |
| **Development** | Indents the records | `POWERTOOLS_DEV` | `false` |
| **TZ** | Time zone, with `WithUTC(false)` | `TZ` | UTC |

An option always wins over its variable. A variable with an invalid value is ignored with a warning, a JSON record at
WARN on standard error; it never stops the function.

## Advanced

### Buffering logs

`WithBuffer` keeps the low-level records of each invocation in a buffer instead of writing them, and writes them only
when something goes wrong: an ERROR record, a failed handler, or an explicit flush.

```go
logger := logging.New(logging.WithService("orders"), logging.WithBuffer())

handle := logger.InjectContext(func(ctx context.Context, id string) (string, error) {
	logger.DebugContext(ctx, "loading order", "id", id) // buffered
	if id == "" {
		logger.ErrorContext(ctx, "no order id") // flushes the buffer first
	}
	return id, nil
})
```

#### Configuring the buffer

| Option | Description | Default |
|---|---|---|
| `BufferMaxBytes` | Bytes of records each invocation buffers | `20480` |
| `BufferAtVerbosity` | Most severe level buffered: DEBUG, INFO or WARN, and the levels below | DEBUG |
| `BufferFlushOnErrorLog` | Flush the buffer before every record at ERROR or above | `true` |

```go
logger := logging.New(logging.WithBuffer(
	logging.BufferAtVerbosity(slog.LevelInfo),
	logging.BufferFlushOnErrorLog(false),
))
```

Buffered records are not filtered by the logger's level: that is the point, a function at INFO gets its DEBUG records
when it fails.

#### Flushing on errors

`logger.FlushBuffer(ctx)` writes the buffered records, `logger.ClearBuffer(ctx)` drops them. With
`WithFlushBufferOnUncaughtError(true)`, `InjectContext` flushes when the handler returns an error or panics.

```go
lambda.Start(logger.InjectContext(handle, logging.WithFlushBufferOnUncaughtError(true)))
```

#### Buffering FAQs

1. **Does the buffer persist across invocations?** No: each invocation has its own, dropped when it ends.
2. **Are records buffered outside invocations?** No. Records logged without the context of an invocation wrapped by
   `InjectContext`, during initialization for instance, are written as usual.
3. **What if the buffer is full?** The oldest records make room for the new one; flushing warns that some were evicted.
4. **How is the size of a record computed?** It is the size of its JSON line, in bytes.
5. **What timestamp does a flushed record have?** The time it was logged, with its location and the keys it had then.
6. **What if a record is larger than the whole buffer?** It is written at once, with a warning.
7. **What if Lambda times out before the buffer is flushed?** The buffered records are lost.
8. **Do two loggers share a buffer?** No: each logger buffers on its own, per invocation.

### Sampling debug logs

`WithSamplingRate` (or `POWERTOOLS_LOGGER_SAMPLE_RATE`) logs at DEBUG a fraction of the invocations, from 0 to 1.
`InjectContext` draws the decision for every invocation; records carry the rate in `sampling_rate`.

```go
var logger = logging.New(logging.WithSamplingRate(0.1))
```

Without `InjectContext`, draw again with `logger.RefreshSampleRateCalculation(ctx)`. Lambda's Advanced Logging Controls
still drop the DEBUG records of a sampled invocation when `AWS_LAMBDA_LOG_LEVEL` is stricter.

### Using the handler with other libraries

The `logging.Handler` is a `slog.Handler`. `slog.SetDefault` sends slog's default logger, and the standard `log`
package, through it; any library logging through slog then writes Powertools records, buffered with the invocation's:

```go
handler := logging.NewHandler(logging.WithService("orders"))
slog.SetDefault(slog.New(handler))
```

It also plugs into libraries that take a handler, such as the durable functions SDK:
`durable.WithLogHandler(logger.Handler())`.

### Customizing the records

| Option | Effect |
|---|---|
| `WithRecordOrder(keys...)` | Keys that open every record, in order (Python's `log_record_order`) |
| `WithKeyName(key, name)` | Renames a Powertools key: `WithKeyName(logging.KeyMessage, "msg")` |
| `WithoutKeys(keys...)` | Leaves keys out: `WithoutKeys(logging.KeyLocation)` |
| `WithReplaceAttr(fn)` | Rewrites or removes any attribute, as `slog.HandlerOptions.ReplaceAttr` |
| `WithIndent(indent)` | Writes records over several lines (four spaces with `POWERTOOLS_DEV`) |
| `WithJSONOptions(opts...)` | Options for `encoding/json/v2`, such as `json.WithMarshalers` (Python's `json_default`) |
| `WithWriter(w)` | Writes somewhere else than standard output |

Python's custom formatters map to these options, or to a `slog.Handler` of your own wrapping the logger's.

```go
logger := logging.New(logging.WithReplaceAttr(func(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "card_number" {
		return slog.String(a.Key, "****")
	}
	return a
}))
```

### Observability providers

Lambda extensions ship the records to other providers with no change to the logger. Datadog parses the default RFC
3339 timestamps, which is all Python's `DatadogLogFormatter` changes.

### Differences from Powertools Python

* Lambda level names and RFC 3339 timestamps by default; Python's are options.
* Per-invocation state lives in the context, so that concurrent invocations never mix: `append_keys` and
  `thread_safe_append_keys` become `AppendKeys`, `append_context_keys` becomes `WithKeys`, and keys end with the
  invocation.
* Buffers belong to the invocation, not to the trace id: Python drops bufferable records logged without a trace id,
  which is the case outside Lambda and with concurrent invocations.
* The X-Ray trace id comes from the OpenTelemetry span or the context first: aws-lambda-go sets `_X_AMZN_TRACE_ID` only
  when it runs one invocation at a time.
* Messages are strings, as in slog: Python's decoding of JSON-looking messages is dropped.
* `exception` is the error's message: Go errors have no traceback. Frames have no source statement, and there are no
  exception notes.
* Not ported, because Go's `log/slog` has no counterpart for them: child loggers and logger reuse by service name (share
  one `Logger`), handler and filter management, `structure_logs`, the deduplication filter and
  `POWERTOOLS_LOG_DEDUPLICATION_DISABLED`, `%`-format overrides of standard keys, `use_datetime_directive`,
  `copy_config_to_registered_loggers` (use `slog.SetDefault`), `set_package_logger` (use `POWERTOOLS_DEBUG`).

## Testing your code

Records are written to the writer of `WithWriter`; `WithClock` fixes their time:

```go
var out strings.Builder
logger := logging.New(
	logging.WithService("payment"),
	logging.WithWriter(&out),
	logging.WithClock(func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }),
)
logger.InfoContext(context.Background(), "collecting payment", "charge_id", "ch_42")
// out.String() holds one JSON record per line.
```

To test a handler wrapped by `InjectContext` as Lambda runs it, give it a context with a Lambda context:

```go
ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{
	AwsRequestID:       "52fdfc07-2182-154f-163f-5f0f9a621d72",
	InvokedFunctionArn: "arn:aws:lambda:eu-west-1:123456789012:function:payment",
})
result, err := logger.InjectContext(handle)(ctx, Payment{ChargeID: "ch_42"})
```

The first invocation of the process is the cold one, whatever the test; leave `cold_start` out with
`WithoutKeys(logging.KeyColdStart)` when a test compares whole records. `WithRandom` replaces the source of the
sampling decisions.
