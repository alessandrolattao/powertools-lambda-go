package logging

import (
	"log/slog"
	"testing"
	"testing/slogtest"

	"github.com/stretchr/testify/require"
)

// TestSlogConformance runs the standard library's tests for slog.Handler
// implementations, with the Powertools keys renamed to slog's.
func TestSlogConformance(t *testing.T) {
	t.Parallel()
	var out *lockedBuffer
	slogtest.Run(t, func(*testing.T) slog.Handler {
		out = &lockedBuffer{}
		return NewHandler(WithWriter(out),
			WithKeyName(KeyMessage, slog.MessageKey), WithKeyName(KeyTimestamp, slog.TimeKey),
			WithoutKeys(KeyService, KeyLocation))
	}, func(t *testing.T) map[string]any {
		t.Helper()
		records := out.records(t)
		require.Len(t, records, 1)
		return records[0]
	})
}
