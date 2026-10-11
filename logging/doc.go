// Package logging is the Powertools structured logger for Lambda, the Go
// side of Powertools for AWS Lambda (Python)'s Logger: JSON records with the
// Lambda context, cold start, correlation ids, debug sampling, per-invocation
// keys and log buffering, built on log/slog.
//
// # Getting started
//
// A Logger is a *slog.Logger with the Powertools helpers. Create it once and
// log with slog's Context methods, which give the record its invocation:
//
//	var logger = logging.New(logging.WithService("payment"))
//
//	func handle(ctx context.Context, order Order) (Receipt, error) {
//		logger.InfoContext(ctx, "collecting payment", "order_id", order.ID)
//		...
//	}
//
//	func main() {
//		lambda.Start(logger.InjectContext(handle))
//	}
//
// Every record is one line of JSON on standard output:
//
//	{"level":"INFO","location":"main.handle:21","message":"collecting payment",
//	 "timestamp":"2026-10-10T12:00:00.123Z","service":"payment","cold_start":true,
//	 "function_name":"payment","function_memory_size":128,
//	 "function_arn":"arn:aws:lambda:eu-west-1:123456789012:function:payment",
//	 "function_request_id":"52fdfc07-2182-154f-163f-5f0f9a621d72","order_id":"o-1",
//	 "xray_trace_id":"1-5759e988-bd862e3fe1be46a994272793"}
//
// The keys are Powertools Python's, so that Go and Python functions share
// log queries: level, location, message, timestamp, service, sampling_rate,
// cold_start, function_name, function_memory_size, function_arn,
// function_request_id, correlation_id, exception, exception_name,
// stack_trace, xray_trace_id (the Key constants).
//
// The Handler underneath is a plain slog.Handler: pass it to slog.SetDefault
// so that libraries logging through slog's default logger (and the standard
// log package) write Powertools records too, or to any library that takes a
// slog.Handler, such as the durable functions SDK
// (durable.WithLogHandler(logger.Handler())).
//
// # The invocation is in the context
//
// Lambda Managed Instances run several invocations at once in one process,
// so nothing that belongs to an invocation lives in the Logger. A record
// takes it from the context it is logged with:
//
//   - function_request_id and function_arn from aws-lambda-go's Lambda
//     context, function_name and function_memory_size from the environment,
//     cold_start from the first invocation the process serves (never with
//     provisioned concurrency);
//   - xray_trace_id from the active OpenTelemetry span, else from the trace
//     header aws-lambda-go stores in the context, else from _X_AMZN_TRACE_ID
//     (set by aws-lambda-go only when it runs one invocation at a time);
//   - the keys of AppendKeys and WithKeys, the correlation id, the sampling
//     decision and the log buffer, from the state InjectContext puts there.
//
// A record logged without the invocation's context (logger.Info instead of
// logger.InfoContext) has none of these.
//
// # Keys
//
// Keys that every record of a logger carries are slog's With:
// logger.With("tenant", "acme"). Keys of one invocation go in its context:
//
//   - AppendKeys(ctx, "order_id", id) adds keys to the rest of the
//     invocation (Python's append_keys); RemoveKeys, ClearState and
//     CurrentKeys go with it;
//   - WithKeys(ctx, "step", "charge") returns a context whose records carry
//     the keys (Python's append_context_keys);
//   - SetCorrelationID and CorrelationID set and read correlation_id.
//
// Keys behave as in a Python dict: a key appears once, a later value
// replaces an earlier one, and a nil value removes the key. The record's own
// attributes win over the logger's, which win over the invocation's, which
// win over the Powertools keys; level, location, message and timestamp
// cannot be overridden.
//
// An error under the "err" key (ErrorKey) is written as exception (its
// message) and exception_name (the name of its type, the first one in the
// wrapped chain that is not the standard library's), plus stack_trace when
// it carries its stack (StackTracer):
//
//	logger.ErrorContext(ctx, "charge failed", "err", err)
//
// # InjectContext
//
// InjectContext wraps the handler with the per-invocation work of Python's
// inject_lambda_context: the invocation's state, the event logged
// (WithLogEvent, POWERTOOLS_LOGGER_LOG_EVENT), the correlation id selected
// with JMESPath (WithCorrelationIDPath and the Correlation constants), the
// buffer flushed when the handler fails (WithFlushBufferOnUncaughtError), a
// panic logged with its stack (WithLogUncaughtPanics) before it goes on.
//
// # Levels and sampling
//
// The level is, in Powertools Python's order: AWS_LAMBDA_LOG_LEVEL (Lambda's
// Advanced Logging Controls, which drop what is below them anyway), then
// WithLevel or SetLevel, then POWERTOOLS_LOG_LEVEL, then LOG_LEVEL, then
// INFO. A configured level more verbose than Lambda's gives a warning.
// LevelTrace and LevelFatal (alias LevelCritical) complete slog's levels.
//
// WithSamplingRate (POWERTOOLS_LOGGER_SAMPLE_RATE) logs at DEBUG a fraction
// of the invocations: InjectContext draws the decision for each one, and
// the records carry the rate in sampling_rate.
//
// # Buffering
//
// WithBuffer keeps the records at or below a level (DEBUG by default) in a
// per-invocation buffer instead of writing them, and writes them, with their
// original time and location, only when the invocation logs an ERROR (by
// default), when the handler fails (WithFlushBufferOnUncaughtError) or when
// FlushBuffer is called. When the buffer is full the oldest records make
// room; a warning says so when it is flushed. Records logged outside an
// invocation are never buffered.
//
// # Formatting
//
// Records carry Lambda's level names and RFC 3339 timestamps with
// milliseconds in UTC, which Lambda's log-level filtering needs. The other
// knobs are options: WithLevelNames(PythonLevelName) and
// WithTimestampFormat(TimestampPython) for Python's output, WithUTC,
// WithRecordOrder, WithKeyName, WithoutKeys, WithReplaceAttr, WithIndent
// (four spaces when POWERTOOLS_DEV is on), WithJSONOptions. Python's custom
// formatters map to these options and to wrapping the Handler in a
// slog.Handler of your own.
//
// # Environment variables
//
//	POWERTOOLS_SERVICE_NAME        service, default "service_undefined"
//	POWERTOOLS_LOG_LEVEL           level, then LOG_LEVEL; default INFO
//	AWS_LAMBDA_LOG_LEVEL           Lambda's Advanced Logging Controls, highest precedence
//	POWERTOOLS_LOGGER_SAMPLE_RATE  debug sampling rate, 0 to 1
//	POWERTOOLS_LOGGER_LOG_EVENT    log the event in InjectContext, default false
//	POWERTOOLS_DEV                 indented records
//	AWS_LAMBDA_FUNCTION_NAME, AWS_LAMBDA_FUNCTION_MEMORY_SIZE, _X_AMZN_TRACE_ID, TZ
//
// An option always wins over its variable. A variable with an invalid value
// is ignored with a warning; it never stops the function. Warnings, like
// Python's, are JSON records at WARN on standard error, once per cause.
//
// # Differences from Powertools Python
//
//   - Lambda level names (WARN, FATAL) and RFC 3339 timestamps by default,
//     since Lambda's log-level filtering files records with Python's names
//     or timestamp format under INFO; Python's are options.
//   - Per-invocation state is in the context. Python's append_keys keys
//     persist across warm invocations; here they end with the invocation,
//     and WithKeys restores the values it shadowed instead of deleting the
//     keys. clear_state is therefore only needed to drop keys a wrapping
//     middleware added (WithClearState).
//   - The Lambda context keys come from each record's context, with or
//     without InjectContext; cold_start appears with them.
//   - Buffers belong to the invocation, not to the trace id: Python drops
//     bufferable records logged without a trace id (outside Lambda, with
//     concurrency above one), here they are written. A buffered record keeps
//     the keys it had when it was logged, and its size is that of its JSON.
//   - The X-Ray trace id comes from the context first, as aws-lambda-go does
//     not set _X_AMZN_TRACE_ID with concurrent invocations.
//   - Messages are strings, as in slog: Python's decoding of JSON-looking
//     messages is dropped; put structured data in attributes. The event that
//     WithLogEvent logs is still the record's message, as a JSON object.
//   - sampling_rate is always a number (Python writes the variable's text).
//   - exception is the error's message: Go errors have no traceback;
//     stack_trace frames have no source statement, and exception_notes does
//     not exist.
//   - Uncaught exceptions are panics: WithLogUncaughtPanics logs them from
//     InjectContext, where Python's sys.excepthook seldom runs in Lambda.
//     Returned errors are not logged, as Python does not log exceptions that
//     propagate.
//   - The function name and memory size are left out when unknown (outside
//     Lambda), where Python writes "UNDEFINED" and 128 for a missing context.
//   - Python's stdlib logging plumbing has no Go counterpart and is not
//     ported: child loggers and logger reuse by service name (share one
//     Logger, or use With), handler and filter management and attribute
//     proxying, structure_logs, the deduplication filter and
//     POWERTOOLS_LOG_DEDUPLICATION_DISABLED, %-format overrides of reserved
//     keys, use_datetime_directive (Go has one layout syntax),
//     copy_config_to_registered_loggers (slog.SetDefault redirects slog's
//     and the log package's default loggers), set_package_logger (Powertools
//     diagnostics follow POWERTOOLS_DEBUG), and InvalidLoggerSamplingRateError
//     (the rate is a float64; an invalid variable warns).
package logging
