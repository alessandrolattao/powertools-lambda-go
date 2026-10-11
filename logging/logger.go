package logging

import (
	"context"
	"log/slog"
)

// Logger is a *slog.Logger writing through a Handler, with the Powertools
// helpers next to slog's methods. Log with slog's Context methods
// (InfoContext, ErrorContext, ...) inside an invocation: the context is
// where the record finds the invocation's Lambda context, keys, sampling
// decision and buffer.
//
// A Logger is safe for concurrent use; create it once, in main or at package
// level, and share it.
type Logger struct {
	*slog.Logger

	handler *Handler
}

// New returns a Logger configured by opts, then by the POWERTOOLS_*
// environment variables, then by Powertools' defaults.
func New(opts ...Option) *Logger {
	return newLogger(NewHandler(opts...))
}

func newLogger(h *Handler) *Logger {
	return &Logger{Logger: slog.New(h), handler: h}
}

// With returns a Logger whose records carry args, key-value pairs or
// slog.Attr values, as slog.Logger.With does: Powertools Python's keys
// given at construction, and append_keys on a logger of its own.
func (l *Logger) With(args ...any) *Logger {
	if len(args) == 0 {
		return l
	}
	return newLogger(l.handler.withAttrs(argsToAttrs(args)))
}

// WithGroup returns a Logger that puts the attributes added from now on,
// and those of each record, in the group name, as slog.Logger.WithGroup
// does.
func (l *Logger) WithGroup(name string) *Logger {
	if name == "" {
		return l
	}
	return newLogger(l.handler.withGroup(name))
}

// FlushBuffer writes the records buffered for the invocation ctx belongs to
// and empties the buffer: Powertools Python's flush_buffer.
func (l *Logger) FlushBuffer(ctx context.Context) error {
	return l.handler.FlushBuffer(ctx)
}

// ClearBuffer drops the records buffered for the invocation ctx belongs to:
// Powertools Python's clear_buffer.
func (l *Logger) ClearBuffer(ctx context.Context) {
	l.handler.ClearBuffer(ctx)
}

// SetLevel changes the minimum level of the records written. Lambda's
// Advanced Logging Controls still take precedence, as with Powertools
// Python's setLevel.
func (l *Logger) SetLevel(level slog.Level) {
	l.handler.SetLevel(level)
}

// Level returns the minimum level of the records written, sampling aside:
// Powertools Python's log_level.
func (l *Logger) Level() slog.Level {
	return l.handler.Level()
}

// Service returns the service name the records carry: Powertools Python's
// name.
func (l *Logger) Service() string {
	return l.handler.Service()
}

// RefreshSampleRateCalculation draws the sampling decision again for the
// invocation ctx belongs to, or for the records logged outside invocations:
// Powertools Python's refresh_sample_rate_calculation. InjectContext draws a
// decision for every invocation on its own.
func (l *Logger) RefreshSampleRateCalculation(ctx context.Context) {
	l.handler.RefreshSampleRateCalculation(ctx)
}
