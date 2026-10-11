package logging

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A goroutine the handler started may outlive it and keep logging while
// the invocation ends and while its sampling is drawn again: the race
// detector must find nothing.
func TestLoggingRacesTheEndOfTheInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithBuffer(), WithSamplingRate(0.5))
	var wg sync.WaitGroup
	h := f.logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
		started := make(chan struct{})
		wg.Go(func() {
			close(started)
			for range 2000 {
				f.logger.DebugContext(ctx, "background work")
			}
		})
		<-started
		for range 2000 {
			f.logger.RefreshSampleRateCalculation(ctx)
		}
		return "", nil
	})
	_, err := h(t.Context(), "event")
	require.NoError(t, err)
	wg.Wait()
}

// flakyWriter fails as many writes as fails says, then keeps the lines.
type flakyWriter struct {
	mu    sync.Mutex
	fails int
	lines []string
}

var errDisk = errors.New("disk full")

func (w *flakyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fails > 0 {
		w.fails--
		return 0, errDisk
	}
	w.lines = append(w.lines, string(p))
	return len(p), nil
}

func TestAFailedFlushKeepsTheErrorRecord(t *testing.T) {
	t.Parallel()
	out := &flakyWriter{}
	l := New(WithWriter(out), WithBuffer(BufferFlushOnErrorLog(true)))
	h := l.InjectContext(func(ctx context.Context, _ string) (string, error) {
		l.DebugContext(ctx, "buffered detail")
		out.mu.Lock()
		out.fails = 1 // the flush of the buffered record fails
		out.mu.Unlock()
		l.Log(ctx, slog.LevelError, "the payment failed")
		return "", nil
	})
	_, err := h(t.Context(), "event")
	require.NoError(t, err)

	out.mu.Lock()
	defer out.mu.Unlock()
	written := strings.Join(out.lines, "")
	assert.Contains(t, written, "the payment failed", "the error record is written even though the flush failed")
}
