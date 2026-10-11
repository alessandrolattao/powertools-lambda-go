// Package metrics creates custom CloudWatch metrics asynchronously, by
// writing them to standard output in the CloudWatch Embedded Metric Format
// (EMF), the way Powertools for AWS Lambda (Python) does. CloudWatch Logs
// extracts the metrics from the function's log group: no API call, no extra
// IAM permission, no latency added to the invocation.
//
// The documents are those Powertools Python writes, byte for byte: same key
// order, same number formatting (1.0, not 1), same escaping of non-ASCII
// characters. A Go function and a Python function feed the same metrics,
// dashboards and alarms.
//
// # Getting started
//
// Create one Metrics per function and decorate the handler with LogMetrics.
// Every invocation gets a metric set of its own, carried by its context;
// AddMetric and the other functions record metrics in it, and LogMetrics
// writes them out when the handler returns, also when it fails or panics:
//
//	var m = metrics.New(metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking"))
//
//	func handle(ctx context.Context, b Booking) (string, error) {
//		if err := metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1); err != nil {
//			return "", err
//		}
//		return b.ID, nil
//	}
//
//	func main() {
//		lambda.Start(m.LogMetrics(handle, metrics.WithColdStartMetric(true)))
//	}
//
// Metrics that share a set share its dimensions: CloudWatch considers a
// metric unique by its namespace, name and dimensions. The functions:
//
//   - AddMetric adds a data point. A name added again collects one more
//     value. The unit is one of the Unit constants (a Python MetricUnit member
//     name, such as "BytesPerSecond", is accepted too); WithResolution(High)
//     makes a one-second high resolution metric.
//   - AddDimension adds a dimension to every metric of the set.
//   - AddDimensions adds a dimension set, one more combination of dimensions
//     the metrics are aggregated by.
//   - AddMetadata adds a value searchable in CloudWatch Logs Insights, which
//     is not a dimension.
//   - SetTimestamp sets the time of the metrics of the set.
//   - Flush writes the set out (LogMetrics does it by itself) and
//     ClearMetrics drops it.
//
// The same functions exist as methods of Metrics, which act on that
// Metrics' set even when the context carries a set of another one.
//
// A set holds at most MaxMetrics metrics and MaxMetrics values per metric:
// reaching either writes the document and starts a new one, with the same
// dimensions and metadata. It holds at most MaxDimensions dimensions, the
// service dimension included.
//
// # Single metrics and the cold start
//
// SingleMetric writes one metric in a document of its own, with dimensions,
// metadata and a timestamp of its own: for a metric whose dimensions must
// not multiply the others, or one that happened at another time.
//
// WithColdStartMetric records a ColdStart metric of value 1 on the first
// invocation of each execution environment, in a document of its own with
// the function_name and service dimensions only. Environments initialized by
// provisioned concurrency have no cold start.
//
// # Configuration
//
// Options of New, each falling back to an environment variable, then to
// Powertools Python's default:
//
//   - WithNamespace: POWERTOOLS_METRICS_NAMESPACE; required to flush.
//   - WithService: POWERTOOLS_SERVICE_NAME; adds the service dimension to
//     every set and to the cold start metric.
//   - WithFunctionName: POWERTOOLS_METRICS_FUNCTION_NAME, then
//     AWS_LAMBDA_FUNCTION_NAME; the function_name dimension of the cold start
//     metric.
//   - WithDefaultDimensions: dimensions every set starts with (also an option
//     of LogMetrics and NewContext); WithoutDefaultDimensions drops them.
//   - WithWriter, WithClock: where documents go (standard output, through
//     the writer every Powertools utility shares, one document per Write
//     call) and how they are timestamped. Tests set both.
//   - WithProvider: another backend, such as package metrics/datadog.
//
// POWERTOOLS_METRICS_DISABLED=true switches metrics off: everything is
// validated, nothing is written. POWERTOOLS_DEV=true does too, unless
// POWERTOOLS_METRICS_DISABLED is set to false. AWS default metrics are not
// affected.
//
// Options of LogMetrics: WithColdStartMetric, WithRaiseOnEmptyMetrics (fail
// the invocation with ErrNoMetrics when it records no metric, instead of
// warning) and WithDefaultDimensions.
//
// # Concurrency and contexts
//
// Lambda Managed Instances run several invocations at once in one process.
// Each one has its own set in its context, so dimensions, metadata and
// timestamps never cross from one to another; goroutines an invocation
// starts with its context record into its set safely. Without LogMetrics,
// NewContext installs a set that Flush writes out (behind the AWS Lambda Web
// Adapter, or in a middleware of your own).
//
// A metric recorded with a context that carries no set (at package
// initialization, or from a goroutine started with context.Background) is
// not lost. Recorded through a method of Metrics, it waits in that Metrics'
// pending set, written with its next invocation or as soon as the set fills
// up. Recorded through a package function, no backend is known yet: it
// waits for the next flush of any set, which writes it in a document of its
// own; at most MaxMetrics such metrics, and as many other calls, wait, and
// beyond that the functions fail with ErrNoMetricSet.
//
// # Errors and warnings
//
// The sentinel errors match Powertools Python's exceptions: ErrMetricName,
// ErrMetricUnit, ErrMetricResolution, ErrMetricValue and ErrSchemaValidation,
// which ErrNoMetrics, ErrNoNamespace and ErrTooManyDimensions wrap. What
// Python reports through its warnings module (a set flushed without metrics,
// a dimension skipped or overwritten, a timestamp CloudWatch will drop) is a
// JSON record at WARN on standard error, once per cause, with the Python
// warning class as category.
//
// # Differences from Powertools Python
//
//   - State lives in the invocation's context, not in class attributes
//     shared by every instance; EphemeralMetrics is not needed, every
//     Metrics is isolated. Namespace, service and default dimensions are
//     configured, not mutated: there are no setters,
//     set_default_dimensions or clear_default_dimensions.
//   - The cold start metric is recorded also when the cold invocation fails
//     or panics, as the TypeScript and Java Powertools do; Python records it
//     only after a successful one. The cold invocation is decided once per
//     process for all the Powertools utilities.
//   - When the handler fails, its error is returned even if the flush fails
//     too (the flush error goes to the POWERTOOLS_DEBUG log); Python raises
//     the flush error instead.
//   - Bugs not carried over: with metrics disabled, Python still prints the
//     cold start document, full documents of 100 metrics and single metrics;
//     a set flushed without metrics keeps its dimensions and metadata for
//     the next invocation, and the timestamp of a set outlives its flush; a
//     single metric that fails validation prints null; full documents are
//     printed with spaces after separators; changing a dimension when 29 are
//     set fails.
//   - Values are float64 and must be finite: Python writes NaN and Infinity,
//     which are not JSON. Dimension values are strings. Metadata is
//     marshalled with encoding/json/v2 when it is added, so later changes to
//     the value do not show and a value that cannot be marshalled is an
//     error right away; map keys are sorted, as Go maps have no order.
//   - single_metric, a context manager, is SingleMetric, which uses the
//     namespace, service and writer of its Metrics and is written by Flush.
//   - serialize_metric_set is not exported: tests read the documents through
//     WithWriter. The legacy MetricManager class is not ported.
//   - Durable functions may replay the code that records metrics: record
//     them inside a step, or once the execution completes.
package metrics
