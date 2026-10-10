package output

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkedWriter splits every Write in two, the way an unsynchronized
// destination would let records interleave.
type chunkedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *chunkedWriter) Write(p []byte) (int, error) {
	half := len(p) / 2
	c.mu.Lock()
	c.buf.Write(p[:half])
	c.mu.Unlock()
	c.mu.Lock()
	c.buf.Write(p[half:])
	c.mu.Unlock()
	return len(p), nil
}

func TestWriterKeepsRecordsWhole(t *testing.T) {
	t.Parallel()
	dst := &chunkedWriter{}
	w := New(dst)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			record := strings.Repeat(string(rune('a'+i%26)), 64) + "\n"
			_, err := w.Write([]byte(record))
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(dst.buf.String(), "\n"), "\n")
	require.Len(t, lines, 50)
	for _, line := range lines {
		assert.Equal(t, strings.Repeat(line[:1], 64), line, "a record was interleaved")
	}
}

func TestFor(t *testing.T) {
	t.Parallel()
	assert.Same(t, Stdout(), For(nil))
	assert.Same(t, Stdout(), For(os.Stdout))

	shared := New(&bytes.Buffer{})
	assert.Same(t, shared, For(shared))

	var buf bytes.Buffer
	_, err := For(&buf).Write([]byte("x\n"))
	require.NoError(t, err)
	assert.Equal(t, "x\n", buf.String())
}
