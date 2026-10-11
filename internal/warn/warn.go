// Package warn is where Powertools tells the developer about a likely
// mistake: what Powertools Python reports through its warnings module, such
// as an invalid environment variable, a utility disabled for development, a
// metric set flushed empty, a Datadog tag that may lose data.
//
// Each warning is a JSON record at WARN on standard error, where Lambda
// collects it, with the keys Lambda's log-level filtering reads (level,
// timestamp, message) and the Python warning class as its category, so that
// a log query matches both runtimes. Unlike internal/debug, warnings are
// always on; each one is written once per process, as Python's default
// warnings filter shows a warning the first time its text appears.
package warn

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/output"
)

// Categories name the Python warning classes.
const (
	UserWarning                  = "UserWarning"
	PowertoolsUserWarning        = "PowertoolsUserWarning"
	DatadogDataValidationWarning = "DatadogDataValidationWarning"
)

// NoMetrics is what a metrics flush without metrics says, as Powertools
// Python says it (with the Go name of the option), for every provider.
const NoMetrics = "No application metrics to publish. The cold-start metric may be published if enabled. " +
	"If application metrics should never be empty, consider using 'WithRaiseOnEmptyMetrics'"

var (
	mu      sync.Mutex
	seen    = map[string]struct{}{}
	handler = newHandler(output.For(os.Stderr))
)

// newHandler writes warnings as JSON records onto w.
func newHandler(w io.Writer) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
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
	}).WithAttrs([]slog.Attr{slog.String("logger", "powertools")})
}

// Once writes the warning msg of category with the attributes args, unless
// the same warning was written before.
func Once(ctx context.Context, category, msg string, args ...any) {
	key := category + "\x00" + msg + "\x00" + fmt.Sprint(args...)
	mu.Lock()
	if _, ok := seen[key]; ok {
		mu.Unlock()
		return
	}
	seen[key] = struct{}{}
	h := handler
	mu.Unlock()

	record := slog.NewRecord(time.Now(), slog.LevelWarn, msg, 0)
	record.Add("category", category)
	record.Add(args...)
	_ = h.Handle(ctx, record) //nolint:errcheck // a warning that cannot be written has nowhere else to go
}

// Capture sends the warnings to w and forgets the ones already written,
// until the returned function restores standard error. Tests use it; they
// cannot run in parallel with each other.
func Capture(w io.Writer) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	previous := handler
	handler = newHandler(w)
	seen = map[string]struct{}{}
	return func() {
		mu.Lock()
		defer mu.Unlock()
		handler = previous
		seen = map[string]struct{}{}
	}
}
