# Metrics

The `metrics` package creates custom metrics asynchronously by writing them to
standard output in the
[Amazon CloudWatch Embedded Metric Format (EMF)](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format.html).
CloudWatch Logs extracts them from the function's log group; they show in the
CloudWatch console like any other metric. The `metrics/datadog` package sends
the same metrics to Datadog instead.

```go
import "github.com/alessandrolattao/powertools-lambda-go/metrics"
```

## Key features

- Aggregate up to 100 metrics in a single CloudWatch EMF document
- Validate against common metric definition mistakes (unit, value, name,
  number of dimensions, number of metrics)
- Metrics are created asynchronously by CloudWatch: no API call, no IAM
  permission, no custom stack
- One metric set per invocation, safe when Lambda Managed Instances run
  several invocations at once in one process
- Documents identical to those of Powertools for AWS Lambda (Python), byte for
  byte: Go and Python functions share metrics, dashboards and alarms
- A one-off metric with its own dimensions or timestamp, and a cold start
  metric
- A Datadog provider, for the Datadog Lambda Extension or the Datadog
  Forwarder

## Terminologies

If you are new to Amazon CloudWatch, there are five terms you should know
before using this utility:

- **Namespace**: the highest level container, grouping the metrics of an
  application, for example `ServerlessEcommerce`.
- **Dimensions**: metadata in key-value form that slices metrics, for example
  the `ColdStart` metric by payment `service`.
- **Metric**: the name of the metric, for example `SuccessfulBooking`.
- **Unit**: the unit of measure, for example `Count` or `Seconds`.
- **Resolution**: the storage resolution, standard (one minute) or high (one
  second). See
  [high-resolution metrics](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/publishingMetrics.html#high-resolution-metrics).

CloudWatch considers a metric unique by its namespace, name and dimensions.

## Getting started

Metrics have two settings used across all the metrics a function emits:

| Setting | Description | Environment variable | Option |
|---|---|---|---|
| Metric namespace | Logical container of all the metrics, for example `ServerlessAirline` | `POWERTOOLS_METRICS_NAMESPACE` | `WithNamespace` |
| Service | Optionally, a `service` dimension on every metric, for example `payment` | `POWERTOOLS_SERVICE_NAME` | `WithService` |

An explicit option wins over the environment variable. Flushing metrics
without a namespace fails with `metrics.ErrNoNamespace`.

!!! tip
    Use your application or main service as the namespace, to group all its
    metrics.

```yaml
# AWS SAM
Globals:
  Function:
    Environment:
      Variables:
        POWERTOOLS_SERVICE_NAME: booking
        POWERTOOLS_METRICS_NAMESPACE: ServerlessAirline
```

### Creating metrics

Create one `Metrics` per function, in `main` or at package level, and decorate
the handler with its `LogMetrics` method. Every invocation gets a metric set of
its own, carried by its context; `metrics.AddMetric` records metrics in it, and
`metrics.AddDimension` adds dimensions to all of them.

```go
var m = metrics.New(metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking"))

func handle(ctx context.Context, b Booking) (string, error) {
	if err := metrics.AddDimension(ctx, "environment", "prod"); err != nil {
		return "", err
	}
	if err := metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1); err != nil {
		return "", err
	}
	return b.ID, nil
}

func main() {
	lambda.Start(m.LogMetrics(handle))
}
```

The units are the `metrics.Unit` constants (`metrics.Count`,
`metrics.Milliseconds`, `metrics.BytesPerSecond`, ...). A unit read from
configuration can be a CloudWatch unit (`"Bytes/Second"`) or the name of a
Python `MetricUnit` member (`"BytesPerSecond"`); `metrics.ParseUnit` checks it.

!!! note "Metrics overflow"
    An EMF document holds at most 100 metrics, and 100 values per metric.
    Adding the hundredth writes the document out right away; the next metrics
    go in a new document with the same dimensions and metadata.

### Adding high-resolution metrics

[High-resolution metrics](https://aws.amazon.com/about-aws/whats-new/2023/02/amazon-cloudwatch-high-resolution-metric-extraction-structured-logs/)
keep one-second data points; they cost more.

```go
err := metrics.AddMetric(ctx, "Latency", metrics.Milliseconds, 12.5, metrics.WithResolution(metrics.High))
```

### Adding multi-value metrics

Adding a metric with the same name again collects one more value:

```go
for _, reads := range []float64{1, 8} {
	if err := metrics.AddMetric(ctx, "TurbineReads", metrics.Count, reads); err != nil {
		return "", err
	}
}
```

```json
{"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[[]],"Metrics":[{"Name":"TurbineReads","Unit":"Count"}]}]},"TurbineReads":[1.0,8.0]}
```

### Adding default dimensions

Dimensions every metric set starts with are options: of `New`, for every
invocation; of `LogMetrics`, for that handler. A dimension with an empty name
or value is skipped with a warning.

```go
var m = metrics.New(
	metrics.WithNamespace("ServerlessAirline"),
	metrics.WithService("booking"),
	metrics.WithDefaultDimensions(metrics.Dimension{Name: "environment", Value: "prod"}),
)

func main() {
	lambda.Start(m.LogMetrics(handle,
		metrics.WithDefaultDimensions(metrics.Dimension{Name: "tier", Value: "gold"})))
}
```

`metrics.WithoutDefaultDimensions()` drops the default dimensions given before
it, as Python's `clear_default_dimensions` does.

### Adding multiple dimension sets

`metrics.AddDimensions` adds a dimension set: one more combination of
dimensions the metrics are aggregated by, in the same document. Each set
starts with the default dimensions.

```go
err := metrics.AddDimensions(ctx,
	metrics.Dimension{Name: "environment", Value: "prod"},
	metrics.Dimension{Name: "region", Value: "us-east-1"})
if err != nil {
	return "", err
}
if err := metrics.AddDimensions(ctx, metrics.Dimension{Name: "environment", Value: "prod"}); err != nil {
	return "", err
}
```

```json
{"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["service"],["environment","region"],["environment"]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"service":"booking","environment":"prod","region":"us-east-1","SuccessfulBooking":[1.0]}
```

### Changing the default timestamp

Metrics are timestamped when they are written. `metrics.SetTimestamp` sets
another time for all the metrics of the set. CloudWatch drops metrics more than
14 days in the past or 2 hours in the future: such a timestamp is kept, with a
warning. For metrics with different timestamps, use
[single metrics](#single-metric).

```go
err := metrics.SetTimestamp(ctx, time.Now().Add(-48*time.Hour))
```

### Flushing metrics

`LogMetrics` validates, serializes and writes the metrics when the handler
returns, also when it returns an error, and when it panics (then the panic goes
on). A set without metrics writes nothing and warns.

The flush fails (and so does the invocation) on a schema problem: no
namespace, more than 29 dimensions with the service one. When the handler
fails too, its own error is the invocation's.

#### Raising ErrNoMetrics on empty metrics

```go
lambda.Start(m.LogMetrics(handle, metrics.WithRaiseOnEmptyMetrics(true)))
```

The invocation then fails with `metrics.ErrNoMetrics` when it records no
metric.

### Capturing the cold start metric

```go
lambda.Start(m.LogMetrics(handle, metrics.WithColdStartMetric(true)))
```

On the first invocation of each execution environment this writes a separate
document with a `ColdStart` metric of value 1 and the `function_name` and
`service` dimensions only, so that the dimensions of the application metrics
do not multiply it:

```json
{"_aws":{"Timestamp":1700000000500,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["function_name","service"]],"Metrics":[{"Name":"ColdStart","Unit":"Count"}]}]},"function_name":"test","service":"booking","ColdStart":[1.0]}
```

No metric is written for warm invocations: zeros would cost without telling
anything. Environments initialized by provisioned concurrency have no cold
start. The cold start is recorded also when the cold invocation fails.

#### Customizing the function name

The `function_name` dimension is, in order of priority:

1. the `WithFunctionName` option;
2. the `POWERTOOLS_METRICS_FUNCTION_NAME` environment variable;
3. the function's name (`AWS_LAMBDA_FUNCTION_NAME`).

### Environment variables

| Setting | Description | Environment variable | Default |
|---|---|---|---|
| Namespace | Namespace of the metrics | `POWERTOOLS_METRICS_NAMESPACE` | none |
| Service | `service` dimension of every metric | `POWERTOOLS_SERVICE_NAME` | none |
| Function name | `function_name` dimension of `ColdStart` | `POWERTOOLS_METRICS_FUNCTION_NAME` | the function's name |
| Disable metrics | Writes nothing, validates everything | `POWERTOOLS_METRICS_DISABLED` | `false` |

`POWERTOOLS_DEV=true` disables metrics too, unless
`POWERTOOLS_METRICS_DISABLED` is explicitly `false`. Neither affects the
default metrics AWS services create.

## Advanced

### Adding metadata

`metrics.AddMetadata` adds high-cardinality data to the document: searchable in
CloudWatch Logs Insights, not a dimension, so it does not create metrics.

```go
err := metrics.AddMetadata(ctx, "booking_id", b.ID)
```

```json
{"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["service"]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"service":"booking","booking_id":"00347014-341d-4b8e-8421-a89d3d588ab3","SuccessfulBooking":[1.0]}
```

The value is marshalled with `encoding/json/v2` when it is added;
`metrics.ErrMetadata` reports one that cannot be.

### Single metric

All the metrics of an EMF document share its dimensions and timestamp.
`SingleMetric` writes one metric in a document of its own, for a metric with
other dimensions or another timestamp. Keep in mind that every unique
combination of name and dimensions is a metric
[you pay for](https://aws.amazon.com/cloudwatch/pricing).

```go
err := m.SingleMetric("RecordsCount", metrics.Count, 10).
	AddDimension("TableName", "Users").
	SetTimestamp(recordTime).
	Flush()
```

The document has the namespace and service of `m`, but none of its default
dimensions unless `AddDefaultDimensions` adds them. The first error of the
chain stops it and is what `Flush` returns.

### Flushing metrics manually

Behind the [AWS Lambda Web Adapter](https://github.com/awslabs/aws-lambda-web-adapter),
or in a middleware of your own, install a metric set with `NewContext` and
write it with `metrics.Flush`, also when the work fails. No cold start metric
is recorded this way.

```go
work := func(ctx context.Context) (err error) {
	ctx = m.NewContext(ctx)
	defer func() { err = errors.Join(err, metrics.Flush(ctx)) }()
	return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
}
```

### Metrics isolation and concurrency

Python shares one metric set between all the `Metrics` instances of a process,
and offers `EphemeralMetrics` for isolated ones. In Go every `Metrics` is
isolated, and every invocation has its own set: goroutines an invocation starts
with its context record into its set, and concurrent invocations never mix
dimensions or metadata.

The package functions act on the innermost set the context carries; the
methods of a `Metrics` (`m.AddMetric`, `m.AddDimension`, ...) on that
`Metrics`' set, even inside another one's `LogMetrics`. For metrics of another
tenant or application with dimensions of their own, nest a second set:

```go
tenantCtx := tenantMetrics.NewContext(ctx)
err := metrics.AddMetric(tenantCtx, "SuccessfulBooking", metrics.Count, 1)
err = errors.Join(err, metrics.Flush(tenantCtx))
```

A metric recorded with a context that carries no set, at package
initialization or from a goroutine started with `context.Background()`, is not
lost:

- through a method of `Metrics`, it waits in that `Metrics`' pending set, and
  is written with its next invocation, by `m.Flush`, or as soon as the set
  fills up;
- through a package function, it waits for the next flush of any set, which
  writes it in a document of its own; at most 100 such metrics (and 100 other
  calls) wait, beyond that the functions fail with `metrics.ErrNoMetricSet`.

### Durable functions

Durable functions may replay the code that records metrics. Record them inside
a step, or once the execution completes.

### Observability providers

`metrics.WithProvider` replaces CloudWatch EMF with another backend implementing
`metrics.Provider`. `LogMetrics`, the cold start metric and the package
functions work the same with any provider.

#### Datadog

```go
import "github.com/alessandrolattao/powertools-lambda-go/metrics/datadog"
```

The Datadog provider sends metrics to the
[Datadog Lambda Extension](https://docs.datadoghq.com/serverless/aws_lambda/)
when the function has it (`/opt/extensions/datadog-agent`), as DogStatsD
distributions over UDP, or writes them to standard output for the
[Datadog Forwarder](https://docs.datadoghq.com/logs/guide/forwarder/).
It does not depend on the Datadog libraries.

```go
var m = datadog.New(datadog.WithNamespace("ServerlessAirline"))

func handle(ctx context.Context, o Order) (string, error) {
	err := datadog.AddMetric(ctx, "item_sold", 1, datadog.WithTag("product", o.Product))
	return o.ID, err
}

func main() {
	lambda.Start(m.LogMetrics(handle, metrics.WithColdStartMetric(true)))
}
```

- **Namespace**: `WithNamespace`, `POWERTOOLS_METRICS_NAMESPACE`, then
  `default`; names are prefixed with the namespace and a dot unless it is
  `default`.
- **Tags**: `WithTag` on a metric, `WithDefaultTag` on the provider, and
  `metrics.WithDefaultDimensions` on `LogMetrics`; the metric's own tags win on
  the same key. Tags that do not start with a letter or exceed 200 characters
  are kept with a warning.
- **Timestamps**: now, to the second, unless `WithTimestamp` says otherwise.
- **Flush to log**: `WithFlushToLog` or `DD_FLUSH_TO_LOG` writes to standard
  output even when the extension is there. Without the extension, metrics go
  to standard output anyway.
- **Cold start**: a `ColdStart` metric tagged with `function_name`, sent with
  the invocation's metrics.
- **Validation**: names start with a letter and hold only ASCII letters,
  digits, `_` and `.`, 200 characters at most; others fail with an error
  wrapping `metrics.ErrSchemaValidation` and `metrics.ErrMetricName`.

```json
{"m":"ServerlessAirline.item_sold","v":1,"e":1700000000,"t":["environment:prod","product:latte"]}
```

Dimensions, metadata, set timestamps and single metrics are CloudWatch
features: with Datadog they fail with an error wrapping
`errors.ErrUnsupported`.

## Testing your code

### Setting the configuration

Pass the namespace and service as options, or set the environment variables in
the test (`t.Setenv("POWERTOOLS_METRICS_NAMESPACE", "ServerlessAirline")`);
without a namespace, flushes fail with `metrics.ErrNoNamespace`.

### Reading the documents

`WithWriter` sends the documents to a buffer and `WithClock` fixes their
timestamp, so that a test compares them exactly:

```go
func TestHandler(t *testing.T) {
	var out bytes.Buffer
	m := metrics.New(
		metrics.WithNamespace("ServerlessAirline"),
		metrics.WithWriter(&out),
		metrics.WithClock(func() time.Time { return time.UnixMilli(1700000000000) }),
	)
	h := m.LogMetrics(handle)

	_, err := h(t.Context(), Booking{ID: "b-1"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[[]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"SuccessfulBooking":[1.0]}`, out.String())
}
```

Every document is one line: with the cold start metric, or with single
metrics, read the output line by line. Each `Metrics` is isolated and each
invocation starts empty: there is nothing to clear between tests. The cold
start is decided once per process, so only the first decorated invocation of a
test binary records it. Datadog tests set `datadog.WithFlushToLog(true)` and
`datadog.WithWriter` to read the Forwarder lines.
