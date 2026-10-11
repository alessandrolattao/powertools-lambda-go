package logging

import (
	"context"
	"encoding/json/jsontext"
	"log/slog"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzHandler logs arbitrary text as message, keys and values: whatever a
// record carries, it is written as exactly one line of valid JSON.
func FuzzHandler(f *testing.F) {
	f.Add("hello", "key", "value", int64(1))
	f.Add("caf\xe9", "", "\x00\n\"", int64(-1))
	f.Add(`{"level":"x"}`, "level", "message", int64(0))
	f.Fuzz(func(t *testing.T, message, key, value string, n int64) {
		out := &lockedBuffer{}
		logger := New(WithWriter(out), WithLevel(LevelTrace)).With(key, value).WithGroup(key)
		r := slog.NewRecord(time.Now(), slog.LevelInfo, message, 0)
		r.AddAttrs(slog.Int64(key, n), slog.Any("nested", map[string]string{key: value}))
		require.NoError(t, logger.Handler().Handle(context.Background(), r))
		lines := out.lines()
		require.Len(t, lines, 1)
		assert.True(t, jsontext.Value(lines[0]).IsValid(), lines[0])
		records := out.records(t)
		if utf8.ValidString(message) {
			assert.Equal(t, message, records[0][KeyMessage])
		}
	})
}

// FuzzTraceRoot reads arbitrary trace headers: the trace id is a part of the
// header, or nothing.
func FuzzTraceRoot(f *testing.F) {
	f.Add(testHeader)
	f.Add("Root=;;=")
	f.Fuzz(func(t *testing.T, header string) {
		root := traceRoot(header)
		assert.Contains(t, header, root)
	})
}

// FuzzParseLevel reads arbitrary level names: a level or ErrInvalidLevel.
func FuzzParseLevel(f *testing.F) {
	f.Add("INFO+2")
	f.Add("warning")
	f.Fuzz(func(t *testing.T, name string) {
		level, err := ParseLevel(name)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidLevel)
			return
		}
		assert.NotEmpty(t, LambdaLevelName(level))
	})
}
