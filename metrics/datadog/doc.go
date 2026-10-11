// Package datadog sends the metrics of package metrics to Datadog, as the
// Datadog provider of Powertools for AWS Lambda (Python) does: to the Datadog
// Lambda Extension when the function has it, or as JSON lines on standard
// output that the Datadog Forwarder picks up from the function's log group.
//
// # Getting started
//
// New returns a metrics.Metrics backed by Datadog. Decorate the handler with
// its LogMetrics method, as with CloudWatch EMF, and add metrics with
// AddMetric, tagging them with WithTag:
//
//	var m = datadog.New(datadog.WithNamespace("ServerlessAirline"))
//
//	func handle(ctx context.Context, o Order) (string, error) {
//		err := datadog.AddMetric(ctx, "item_sold", 1, datadog.WithTag("product", o.Product))
//		return o.ID, err
//	}
//
//	func main() {
//		lambda.Start(m.LogMetrics(handle, metrics.WithColdStartMetric(true)))
//	}
//
// metrics.AddMetric works too; Datadog ignores the unit and the resolution.
// Each data point is timestamped when it is added, to the second, unless
// WithTimestamp says otherwise. Default tags (WithDefaultTag, and
// metrics.WithDefaultDimensions given to LogMetrics) go under the metric's
// own tags, which win on the same key.
//
// Metric names must start with a letter and hold only ASCII letters, digits,
// underscores and periods, 200 characters at most: other names fail with an
// error wrapping both metrics.ErrSchemaValidation and metrics.ErrMetricName.
// Tags that do not start with a letter, or are longer than 200 characters,
// are kept with a DatadogDataValidationWarning, as Datadog may drop them.
// The cold start metric is a ColdStart metric of value 1 tagged with
// function_name, sent with the invocation's metrics.
//
// # Configuration
//
//   - WithNamespace: POWERTOOLS_METRICS_NAMESPACE, then DefaultNamespace;
//     metric names are prefixed with the namespace and a dot, unless it is
//     the default one.
//   - WithFlushToLog: DD_FLUSH_TO_LOG; write to standard output for the
//     Forwarder even when the extension is there.
//   - WithFunctionName: POWERTOOLS_METRICS_FUNCTION_NAME, then
//     AWS_LAMBDA_FUNCTION_NAME; the function_name tag of the cold start
//     metric.
//   - WithDefaultTag, WithWriter, WithClock.
//
// POWERTOOLS_METRICS_DISABLED and POWERTOOLS_DEV switch metrics off as they
// do for CloudWatch EMF, whatever the destination.
//
// # The Datadog Lambda Extension
//
// Powertools Python hands the metrics to datadog-lambda's lambda_metric,
// which sends them to the extension as DogStatsD distributions. This package
// does the same without depending on the Datadog libraries (datadog-lambda-go
// pulls in the whole tracer): when /opt/extensions/datadog-agent exists, each
// metric is a distribution datagram sent over UDP to the extension at
// 127.0.0.1:8125, its tags normalized as datadog-lambda normalizes them. A
// metric whose timestamp was set explicitly carries it (the extension takes
// historical metrics) and is skipped with a warning when older than 4 hours,
// as datadog-lambda skips it; the others are timestamped by the extension
// when they arrive. A metric that cannot be delivered is reported with a
// warning, never as an error of the invocation.
//
// # Differences from Powertools Python
//
//   - Without the extension, metrics go to standard output whatever
//     DD_FLUSH_TO_LOG says. Python with datadog-lambda installed would submit
//     them to the Datadog API instead, which needs the API key handling this
//     port leaves to Datadog's own libraries.
//   - POWERTOOLS_METRICS_DISABLED switches off the extension path too;
//     Python sends the metrics to the extension anyway.
//   - The dd_lambda_layer tag datadog-lambda adds to identify itself is not
//     added.
//   - Default tags are configured (WithDefaultTag, or LogMetrics options),
//     not mutated: there is no set_default_tags or clear_default_tags.
//   - Values are float64 and must be finite; tags are strings.
//   - CloudWatch-only operations (dimensions, metadata, set timestamps,
//     single metrics) fail with an error wrapping errors.ErrUnsupported.
package datadog
