// Package env reads the environment variables Powertools is configured with.
//
// The names and the accepted values are those of Powertools for AWS Lambda
// (Python), so that a function keeps its configuration when it moves from one
// runtime to the other. An explicit option always wins over the variable,
// which wins over the default.
package env

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// The variables every utility reads.
const (
	ServiceName = "POWERTOOLS_SERVICE_NAME"
	Dev         = "POWERTOOLS_DEV"
	Debug       = "POWERTOOLS_DEBUG"

	LogLevel          = "POWERTOOLS_LOG_LEVEL"
	LegacyLogLevel    = "LOG_LEVEL"
	LambdaLogLevel    = "AWS_LAMBDA_LOG_LEVEL"
	LambdaLogFormat   = "AWS_LAMBDA_LOG_FORMAT"
	LoggerSampleRate  = "POWERTOOLS_LOGGER_SAMPLE_RATE"
	LoggerLogEvent    = "POWERTOOLS_LOGGER_LOG_EVENT"
	LogDeduplication  = "POWERTOOLS_LOG_DEDUPLICATION_DISABLED"
	TracerCaptureResp = "POWERTOOLS_TRACER_CAPTURE_RESPONSE"
	TracerCaptureErr  = "POWERTOOLS_TRACER_CAPTURE_ERROR"
	TraceDisabled     = "POWERTOOLS_TRACE_DISABLED"
	TraceMiddlewares  = "POWERTOOLS_TRACE_MIDDLEWARES"

	MetricsNamespace    = "POWERTOOLS_METRICS_NAMESPACE"
	MetricsDisabled     = "POWERTOOLS_METRICS_DISABLED"
	MetricsFunctionName = "POWERTOOLS_METRICS_FUNCTION_NAME"
	DatadogFlushToLog   = "DD_FLUSH_TO_LOG"

	ParametersMaxAge     = "POWERTOOLS_PARAMETERS_MAX_AGE"
	ParametersSSMDecrypt = "POWERTOOLS_PARAMETERS_SSM_DECRYPT"

	IdempotencyDisabled    = "POWERTOOLS_IDEMPOTENCY_DISABLED"
	CircuitBreakerDisabled = "POWERTOOLS_CIRCUIT_BREAKER_DISABLED"

	LambdaFunctionName       = "AWS_LAMBDA_FUNCTION_NAME"
	LambdaFunctionVersion    = "AWS_LAMBDA_FUNCTION_VERSION"
	LambdaFunctionMemorySize = "AWS_LAMBDA_FUNCTION_MEMORY_SIZE"
	LambdaInitializationType = "AWS_LAMBDA_INITIALIZATION_TYPE"
	LambdaTaskRoot           = "LAMBDA_TASK_ROOT"
	LambdaMetadataAPI        = "AWS_LAMBDA_METADATA_API"
	LambdaMetadataToken      = "AWS_LAMBDA_METADATA_TOKEN" //nolint:gosec // G101: the name of a variable, not a credential
	Region                   = "AWS_REGION"
	XRayTraceID              = "_X_AMZN_TRACE_ID"
	SAMLocal                 = "AWS_SAM_LOCAL"
)

// DefaultServiceName is the service of a function that does not name one, as
// Powertools Python calls it.
const DefaultServiceName = "service_undefined"

// Get returns the value of the variable name, without surrounding spaces.
func Get(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// Bool reads the variable name as a truth value, accepting what Powertools
// Python accepts: 1, y, yes, t, true, on and 0, n, no, f, false, off, in any
// case. An unset variable gives fallback; a value that is neither warns once
// and gives fallback too, so a typo never stops a function from starting.
func Bool(name string, fallback bool) bool {
	raw := Get(name)
	if raw == "" {
		return fallback
	}
	value, ok := ParseBool(raw)
	if !ok {
		warnInvalid(name, raw)
		return fallback
	}
	return value
}

// ParseBool parses a truth value the way Powertools Python's strtobool does.
func ParseBool(raw string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "y", "yes", "t", "true", "on":
		return true, true
	case "0", "n", "no", "f", "false", "off":
		return false, true
	default:
		return false, false
	}
}

// Float reads the variable name as a number, or gives fallback.
func Float(name string, fallback float64) float64 {
	raw := Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		warnInvalid(name, raw)
		return fallback
	}
	return value
}

// Seconds reads the variable name as a whole number of seconds, or gives
// fallback.
func Seconds(name string, fallback time.Duration) time.Duration {
	raw := Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		warnInvalid(name, raw)
		return fallback
	}
	return time.Duration(value) * time.Second
}

// ServiceNameOr returns POWERTOOLS_SERVICE_NAME, or fallback when it is unset.
func ServiceNameOr(fallback string) string {
	if name := Get(ServiceName); name != "" {
		return name
	}
	return fallback
}

// IsDev reports whether POWERTOOLS_DEV is on: utilities trade compactness for
// readability (pretty JSON logs, verbose errors).
func IsDev() bool {
	return Bool(Dev, false)
}

// IsDebug reports whether POWERTOOLS_DEBUG is on: Powertools writes down what
// it does internally.
func IsDebug() bool {
	return Bool(Debug, false)
}

// IsProvisioned reports whether the execution environment was initialized by
// provisioned concurrency, whose invocations never pay a cold start.
func IsProvisioned() bool {
	return Get(LambdaInitializationType) == "provisioned-concurrency"
}

// warnInvalid reports a variable whose value cannot be read. It goes to
// standard error, where Lambda collects it, as a JSON record.
func warnInvalid(name, raw string) {
	slog.New(slog.NewJSONHandler(os.Stderr, nil)).Warn("powertools: ignoring an invalid environment variable",
		"variable", name, "value", raw)
}
