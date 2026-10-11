// Package warn reports the soft problems Powertools Python reports through
// its warnings module: a metric set flushed empty, a dimension skipped, a
// timestamp CloudWatch will drop, a Datadog tag that may lose data.
//
// Each one becomes a JSON record at WARN on standard error, where Lambda
// collects it, written once per cause: Python's default warnings filter
// shows a warning the first time its text appears, and a function that
// flushes no metrics on every invocation should not repeat itself forever.
package warn

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Categories name the Python warning classes, so that a log query can match
// the records of both runtimes.
const (
	UserWarning                  = "UserWarning"
	PowertoolsUserWarning        = "PowertoolsUserWarning"
	DatadogDataValidationWarning = "DatadogDataValidationWarning"
)

// NoMetrics is what a flush without metrics says, as Powertools Python says
// it (with the Go name of the option).
const NoMetrics = "No application metrics to publish. The cold-start metric may be published if enabled. " +
	"If application metrics should never be empty, consider using 'WithRaiseOnEmptyMetrics'"

var (
	mu      sync.Mutex
	seen                 = map[string]struct{}{}
	handler slog.Handler = slog.NewJSONHandler(os.Stderr, nil)
)

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

// Capture sends the warnings to w, as JSON records, and forgets the ones
// already written, until the returned function restores standard error.
// Tests use it; they cannot run in parallel with each other.
func Capture(w io.Writer) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	previous := handler
	handler = slog.NewJSONHandler(w, nil)
	seen = map[string]struct{}{}
	return func() {
		mu.Lock()
		defer mu.Unlock()
		handler = previous
		seen = map[string]struct{}{}
	}
}
