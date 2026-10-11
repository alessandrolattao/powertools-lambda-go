package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messages returns the message of every record written.
func (f *fixture) messages(t *testing.T) []string {
	t.Helper()
	records := f.out.records(t)
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, fmt.Sprint(record[KeyMessage]))
	}
	return out
}

func TestBufferAtVerbosity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		opts    []BufferOption
		flushed []string
		direct  []string
	}{
		{
			name:    "DEBUG by default",
			flushed: []string{"trace", "debug"},
			direct:  []string{"info", "warn"},
		},
		{
			name:    "INFO",
			opts:    []BufferOption{BufferAtVerbosity(slog.LevelInfo)},
			flushed: []string{"trace", "debug", "info"},
			direct:  []string{"warn"},
		},
		{
			name:    "WARN",
			opts:    []BufferOption{BufferAtVerbosity(slog.LevelWarn)},
			flushed: []string{"trace", "debug", "info", "warn"},
			direct:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, WithBuffer(tc.opts...))
			f.invoke(t.Context(), func(ctx context.Context) {
				f.logger.Log(ctx, LevelTrace, "trace")
				f.logger.DebugContext(ctx, "debug")
				f.logger.InfoContext(ctx, "info")
				f.logger.WarnContext(ctx, "warn")
				assert.Equal(t, tc.direct, f.messages(t), "written before the flush")
				require.NoError(t, f.logger.FlushBuffer(ctx))
			})
			assert.Equal(t, append(tc.direct, tc.flushed...), f.messages(t))
		})
	}
}

func TestBufferedRecordsKeepTheirTimeAndKeys(t *testing.T) {
	t.Parallel()
	var tick atomic.Int64
	clock := func() time.Time { return testTime.Add(time.Duration(tick.Add(1)) * time.Second) }
	f := newFixture(t, WithBuffer(), WithClock(clock))
	f.invoke(t.Context(), func(ctx context.Context) {
		AppendKeys(ctx, "step", 1)
		f.logger.DebugContext(ctx, "first")
		AppendKeys(ctx, "step", 2)
		f.logger.ErrorContext(ctx, "failed")
	})
	records := f.out.records(t)
	require.Len(t, records, 2)
	assert.Equal(t, "first", records[0][KeyMessage])
	assert.Equal(t, "2026-10-10T12:00:01.123Z", records[0][KeyTimestamp])
	assert.InDelta(t, 1, records[0]["step"], 0)
	assert.Regexp(t, `^logging\.TestBufferedRecordsKeepTheirTimeAndKeys\.func2:\d+$`, records[0][KeyLocation])
	assert.Equal(t, "2026-10-10T12:00:02.123Z", records[1][KeyTimestamp])
}

func TestBufferFlushOnErrorLog(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer())
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "one")
		f.logger.DebugContext(ctx, "two")
		f.logger.ErrorContext(ctx, "failed")
		f.logger.DebugContext(ctx, "three")
	})
	assert.Equal(t, []string{"one", "two", "failed"}, f.messages(t), "three is dropped when the invocation ends")

	f = newFixture(t, WithBuffer(BufferFlushOnErrorLog(false)))
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "one")
		f.logger.Log(ctx, LevelFatal, "fatal")
	})
	assert.Equal(t, []string{"fatal"}, f.messages(t))
}

func TestBufferFlushesForErrorsBelowTheLevel(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer(), WithLevel(LevelFatal))
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "one")
		f.logger.ErrorContext(ctx, "below the level")
	})
	assert.Equal(t, []string{"one"}, f.messages(t), "Python flushes on error() even when the error is filtered")
}

func TestClearBuffer(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer())
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "dropped")
		f.logger.ClearBuffer(ctx)
		f.logger.DebugContext(ctx, "kept")
		require.NoError(t, f.logger.FlushBuffer(ctx))
		require.NoError(t, f.logger.FlushBuffer(ctx), "an empty buffer flushes nothing")
	})
	assert.Equal(t, []string{"kept"}, f.messages(t))
	f.logger.ClearBuffer(t.Context())
	require.NoError(t, f.logger.FlushBuffer(t.Context()))
}

func TestBufferEviction(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation), WithBuffer(BufferMaxBytes(300)))
	f.invoke(t.Context(), func(ctx context.Context) {
		for n := range 5 {
			f.logger.DebugContext(ctx, "m", "n", n) // each line is 98 bytes: three fit
		}
		require.NoError(t, f.logger.FlushBuffer(ctx))
	})
	records := f.out.records(t)
	kept := make([]any, 0, len(records))
	for _, record := range records {
		kept = append(kept, record["n"])
	}
	assert.Equal(t, []any{2.0, 3.0, 4.0}, kept)
	assert.Contains(t, f.warnings.String(), "Some logs are not displayed because they were evicted from the buffer")
}

func TestBufferOversizedRecord(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer(BufferMaxBytes(200)))
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "small")
		f.logger.DebugContext(ctx, "large", "payload", strings.Repeat("x", 300))
		assert.Len(t, f.out.lines(), 1, "too big for the buffer: written at once")
	})
	assert.Contains(t, f.warnings.String(), "Cannot add item to the buffer. Item size exceeds total cache size 200 bytes")
}

func TestBufferOutsideAnInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer())
	f.logger.DebugContext(t.Context(), "dropped by the level")
	f.logger.InfoContext(t.Context(), "written")
	assert.Equal(t, []string{"written"}, f.messages(t), "nothing is buffered, nothing is lost to a missing trace id")
}

func TestBufferAfterTheInvocationEnds(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer(), WithLevel(slog.LevelDebug))
	ended, logged := make(chan struct{}), make(chan struct{})
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "dropped with the invocation")
		go func() { // outlives the invocation
			<-ended
			f.logger.DebugContext(ctx, "written directly")
			close(logged)
		}()
	})
	close(ended)
	<-logged
	assert.Equal(t, []string{"written directly"}, f.messages(t))
}

func TestBufferFlushOnUncaughtError(t *testing.T) {
	t.Parallel()
	errDeclined := errors.New("declined")
	for _, flush := range []bool{true, false} {
		f := newFixture(t, WithBuffer())
		handle := f.logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
			f.logger.DebugContext(ctx, "before the failure")
			return "", errDeclined
		}, WithFlushBufferOnUncaughtError(flush))
		_, err := handle(t.Context(), "")
		require.ErrorIs(t, err, errDeclined)
		if flush {
			assert.Equal(t, []string{"before the failure"}, f.messages(t))
		} else {
			assert.Empty(t, f.messages(t))
		}
	}
}

func TestBufferFlushOnUncaughtPanic(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer())
	handle := f.logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
		f.logger.DebugContext(ctx, "before the panic")
		panic("boom")
	}, WithFlushBufferOnUncaughtError(true))
	assert.PanicsWithValue(t, "boom", func() { _, _ = handle(t.Context(), "") }) //nolint:errcheck // it panics
	assert.Equal(t, []string{"before the panic"}, f.messages(t))
}

func TestBufferIsPerLoggerAndInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer())
	other := newFixture(t, WithBuffer())
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "mine")
		other.logger.DebugContext(ctx, "other's")
		require.NoError(t, other.logger.FlushBuffer(ctx))
	})
	assert.Empty(t, f.messages(t))
	assert.Equal(t, []string{"other's"}, other.messages(t))
}

func TestBufferConcurrentGoroutinesOfOneInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer(BufferMaxBytes(1<<20)))
	f.invoke(t.Context(), func(ctx context.Context) {
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				f.logger.DebugContext(ctx, "work")
			})
		}
		wg.Wait()
		f.logger.ErrorContext(ctx, "failed")
	})
	assert.Len(t, f.messages(t), 21)
}

func TestBufferWarnings(t *testing.T) {
	t.Setenv("AWS_LAMBDA_LOG_LEVEL", "INFO")
	f := newFixture(t, WithBuffer(BufferMaxBytes(0), BufferAtVerbosity(slog.LevelError)))
	for _, warning := range []string{
		"Ignoring a buffer size of 0 bytes: it must be positive.",
		"Ignoring buffering at ERROR: records at ERROR and above are never buffered.",
		"Advanced Logging Controls (ALC) Log Level is less verbose than Log Buffering Log Level. Buffered logs will be filtered by ALC",
	} {
		assert.Contains(t, f.warnings.String(), warning)
	}
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "buffered at the default level")
		require.NoError(t, f.logger.FlushBuffer(ctx))
	})
	assert.Equal(t, []string{"buffered at the default level"}, f.messages(t))
	assert.Contains(t, f.warnings.String(), "Some logs might be missing")
}

func TestBufferEvictsOldestFirst(t *testing.T) {
	t.Parallel()
	b := newBuffer(10)
	assert.True(t, b.add([]byte("aaaa")))
	assert.True(t, b.add([]byte("bbbb")))
	assert.True(t, b.add([]byte("cccc")))
	assert.False(t, b.add([]byte("too long line")))
	lines, evicted := b.drain()
	assert.True(t, evicted)
	assert.Equal(t, [][]byte{[]byte("bbbb"), []byte("cccc")}, lines)
	lines, evicted = b.drain()
	assert.False(t, evicted)
	assert.Empty(t, lines)
}
