// Package output is where Powertools writes the records Lambda collects: log
// lines, EMF documents, Datadog metrics.
//
// Lambda Managed Instances run several invocations at once in one process,
// all writing to the same standard output. A record written in more than one
// Write call, or larger than what a pipe writes atomically, could interleave
// with another invocation's and corrupt both. Every Powertools writer goes
// through a Writer, which hands each record to the destination in one
// Write call, one record at a time.
package output

import (
	"io"
	"os"
	"sync"
)

// Writer serializes records onto an io.Writer.
type Writer struct {
	mu  sync.Mutex
	dst io.Writer
}

// New returns a Writer onto dst.
func New(dst io.Writer) *Writer {
	return &Writer{dst: dst}
}

// Write writes one record, whole, while no other record is being written.
// The caller passes the complete record, newline included.
func (w *Writer) Write(record []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(record) //nolint:wrapcheck // a Writer is transparent: callers see the destination's errors
}

// Stdout is the Writer onto the process's standard output, shared by every
// utility that writes there.
var Stdout = sync.OnceValue(func() *Writer { return New(os.Stdout) })

// For returns a Writer onto w. Writers onto standard output share one lock;
// a Writer passed in is used as it is.
func For(w io.Writer) io.Writer {
	switch v := w.(type) {
	case nil:
		return Stdout()
	case *Writer:
		return v
	case *os.File:
		if v == os.Stdout {
			return Stdout()
		}
	}
	return New(w)
}
