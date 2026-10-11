package logging

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"time"
)

// Timestamp layouts for WithTimestampFormat.
const (
	// TimestampRFC3339Milli is the default: RFC 3339 with milliseconds, the
	// format Lambda's log-level filtering requires of a JSON record's
	// timestamp (it files a record without one under INFO).
	TimestampRFC3339Milli = "2006-01-02T15:04:05.000Z07:00"
	// TimestampPython is Powertools Python's default, 2021-05-03
	// 11:47:12,494+0000. Lambda's log-level filtering does not read it.
	TimestampPython = "2006-01-02 15:04:05,000-0700"
)

// Option configures a Handler or a Logger.
type Option func(*config)

// config is what the options set; NewHandler resolves it against the
// environment.
type config struct {
	service      *string
	level        *slog.Level
	samplingRate *float64
	writer       io.Writer
	buffer       *bufferConfig
	logPanics    bool
	stackTrace   bool
	timeLayout   string
	utc          bool
	levelName    func(slog.Level) string
	order        []string
	names        map[string]string
	dropped      map[string]bool
	replaceAttr  func(groups []string, a slog.Attr) slog.Attr
	indent       *string
	jsonOptions  []json.Options
	clock        func() time.Time
	random       func() float64

	// Seams for the tests of this package.
	warnings  io.Writer
	coldStart func(context.Context) bool
	function  *functionInfo
}

func defaultConfig() config {
	return config{
		stackTrace: true,
		timeLayout: TimestampRFC3339Milli,
		utc:        true,
		levelName:  LambdaLevelName,
		order:      []string{KeyLevel, KeyLocation, KeyMessage, KeyTimestamp},
		warnings:   os.Stderr,
	}
}

// WithService names the service every record carries in the service key.
// It defaults to POWERTOOLS_SERVICE_NAME, then to "service_undefined".
func WithService(name string) Option {
	return func(c *config) { c.service = &name }
}

// WithLevel sets the minimum level of the records written. Lambda's
// Advanced Logging Controls (AWS_LAMBDA_LOG_LEVEL) take precedence, as in
// Powertools Python: Lambda drops the records below them anyway. Without
// this option the level comes from POWERTOOLS_LOG_LEVEL, then LOG_LEVEL,
// then defaults to INFO.
func WithLevel(level slog.Level) Option {
	return func(c *config) { c.level = &level }
}

// WithSamplingRate logs at DEBUG the given fraction of invocations, from 0
// (none) to 1 (all): each invocation that InjectContext wraps draws its own
// decision. Records carry the rate in the sampling_rate key. It defaults to
// POWERTOOLS_LOGGER_SAMPLE_RATE.
func WithSamplingRate(rate float64) Option {
	return func(c *config) { c.samplingRate = &rate }
}

// WithWriter writes the records to w instead of standard output. Every
// record is written in a single Write call.
func WithWriter(w io.Writer) Option {
	return func(c *config) { c.writer = w }
}

// WithLogUncaughtPanics makes InjectContext log a panicking handler at
// ERROR, with the panic's stack, before the panic goes on: Powertools
// Python's log_uncaught_exceptions. It is off by default.
func WithLogUncaughtPanics(enabled bool) Option {
	return func(c *config) { c.logPanics = enabled }
}

// WithStackTrace controls the stack_trace key of panics and of errors that
// carry their stack (see StackTracer). It is on by default, as Powertools
// Python's serialize_stacktrace.
func WithStackTrace(enabled bool) Option {
	return func(c *config) { c.stackTrace = enabled }
}

// WithTimestampFormat sets the time layout of the timestamp key, in the
// syntax of time.Format. It defaults to TimestampRFC3339Milli.
func WithTimestampFormat(layout string) Option {
	return func(c *config) { c.timeLayout = layout }
}

// WithUTC writes timestamps in UTC when true, the default, and in the local
// time zone (the TZ variable) when false, as Powertools Python's default.
// Lambda runs functions in UTC unless TZ says otherwise.
func WithUTC(utc bool) Option {
	return func(c *config) { c.utc = utc }
}

// WithLevelNames sets how levels are named in the level key. It defaults to
// LambdaLevelName; PythonLevelName gives Powertools Python's names.
func WithLevelNames(name func(slog.Level) string) Option {
	return func(c *config) { c.levelName = name }
}

// WithRecordOrder sets the keys that open every record, in order: Powertools
// Python's log_record_order. The other keys follow in their usual order. It
// defaults to level, location, message, timestamp.
func WithRecordOrder(keys ...string) Option {
	return func(c *config) { c.order = keys }
}

// WithKeyName renames one of the keys Powertools writes (the Key constants):
// WithKeyName(KeyMessage, "msg").
func WithKeyName(key, name string) Option {
	return func(c *config) {
		if c.names == nil {
			c.names = map[string]string{}
		}
		c.names[key] = name
	}
}

// WithoutKeys leaves keys out of every record, whoever sets them: the
// Powertools ones (WithoutKeys(KeyLocation, KeyXRayTraceID)) or your own.
func WithoutKeys(keys ...string) Option {
	return func(c *config) {
		if c.dropped == nil {
			c.dropped = map[string]bool{}
		}
		for _, key := range keys {
			c.dropped[key] = true
		}
	}
}

// WithReplaceAttr rewrites or removes attributes before they are written,
// as slog.HandlerOptions.ReplaceAttr does: it sees every non-group
// attribute, the Powertools keys included. The level arrives as a slog.Level
// and the timestamp as a time.Time; returned unchanged, they are formatted
// as configured. Returning an empty Attr removes the attribute.
func WithReplaceAttr(fn func(groups []string, a slog.Attr) slog.Attr) Option {
	return func(c *config) { c.replaceAttr = fn }
}

// WithIndent writes each record over several lines, indented by indent. It
// defaults to four spaces when POWERTOOLS_DEV is on, and to one line per
// record otherwise, the shape CloudWatch Logs expects.
func WithIndent(indent string) Option {
	return func(c *config) { c.indent = &indent }
}

// WithJSONOptions passes options to encoding/json/v2 when it encodes
// attribute values, for example json.WithMarshalers to control how a type is
// written: Powertools Python's json_default. A value that cannot be encoded
// is written as its fmt.Sprint text.
func WithJSONOptions(opts ...json.Options) Option {
	return func(c *config) { c.jsonOptions = append(c.jsonOptions, opts...) }
}

// WithClock replaces the time of every record with what now returns, for
// tests and examples whose output must not change.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.clock = now }
}

// WithRandom replaces the source of the sampling decisions: a function
// returning a number in [0, 1), safe for concurrent use. Tests use it.
func WithRandom(random func() float64) Option {
	return func(c *config) { c.random = random }
}

// WithBuffer buffers the low-level records of each invocation instead of
// writing them, to flush them only when something goes wrong: Powertools
// Python's log buffering. Without options it buffers DEBUG records, up to
// 20480 bytes per invocation, and flushes on any ERROR record.
func WithBuffer(opts ...BufferOption) Option {
	return func(c *config) {
		buffer := defaultBufferConfig()
		for _, opt := range opts {
			opt(&buffer)
		}
		c.buffer = &buffer
	}
}

// BufferOption configures WithBuffer.
type BufferOption func(*bufferConfig)

type bufferConfig struct {
	maxBytes     int
	at           slog.Level
	flushOnError bool
}

func defaultBufferConfig() bufferConfig {
	return bufferConfig{maxBytes: 20480, at: slog.LevelDebug, flushOnError: true}
}

// BufferMaxBytes sets how many bytes of records an invocation buffers. When
// a record does not fit, the oldest ones make room for it; a record larger
// than the whole buffer is written at once. It defaults to 20480.
func BufferMaxBytes(n int) BufferOption {
	return func(b *bufferConfig) { b.maxBytes = n }
}

// BufferAtVerbosity sets the most severe level buffered: DEBUG (the
// default), INFO or WARN, and every level below it. ERROR and above are
// never buffered.
func BufferAtVerbosity(level slog.Level) BufferOption {
	return func(b *bufferConfig) { b.at = level }
}

// BufferFlushOnErrorLog controls whether a record at ERROR or above flushes
// the buffer before it is written. It is on by default.
func BufferFlushOnErrorLog(enabled bool) BufferOption {
	return func(b *bufferConfig) { b.flushOnError = enabled }
}

// functionInfo is what the logger tells of the function itself.
type functionInfo struct {
	name   string
	memory int
}
