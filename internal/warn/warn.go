// Package warn is where Powertools tells the developer about a likely
// mistake: what Powertools Python reports through the warnings module, such
// as an invalid environment variable or a utility disabled for development.
//
// Warnings are JSON records at WARN on standard error, where Lambda collects
// them, with the keys Lambda's log-level filtering reads. Unlike
// internal/debug they are always on, and each cause is reported once per
// process, as Python's default warning filter does.
package warn

import (
	"log/slog"
	"os"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/internal/output"
)

// seen holds the causes already reported.
var seen sync.Map

// logger writes the warnings.
var logger = sync.OnceValue(func() *slog.Logger {
	return slog.New(slog.NewJSONHandler(output.For(os.Stderr), &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				a.Key = "timestamp"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	})).With("logger", "powertools")
})

// Once reports msg the first time cause is seen in this process. The cause
// identifies the problem (a variable name and its value, a utility being
// disabled), so the same mistake is not repeated on every invocation.
func Once(cause, msg string, args ...any) {
	if _, loaded := seen.LoadOrStore(cause, struct{}{}); loaded {
		return
	}
	logger().Warn(msg, args...) //nolint:sloglint // msg is a constant at every call site
}
