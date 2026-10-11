// Package lambdahttp carries an HTTP request built from a Lambda event
// through an http.Handler and collects the response, for the event source
// adapters of the eventhandler packages.
//
// The handler writes into a ResponseBuffer, since a Lambda proxy response is
// built only once the handler is done. An error the handler could not turn
// into a response travels back through the Invocation stored in the request
// context, so that the adapter returns it to Lambda and the invocation fails,
// as Powertools Python re-raises unhandled exceptions.
package lambdahttp

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
)

// Invocation is what an adapter knows about the Lambda invocation behind a
// request. The adapter creates it, the router reads it and records the
// unhandled error in it.
type Invocation struct {
	// Event is a pointer to the Lambda event the request was built from.
	Event any
	// BasePath is the path prefix the API is served under, "/{stage}" for
	// an API Gateway stage URL, used for the servers of the OpenAPI
	// document.
	BasePath string
	// TrailingSlash makes routes ignore trailing slashes, as Powertools
	// Python's REST resolver does.
	TrailingSlash bool
	// CommaQuery marks sources that fold the values of a repeated query
	// parameter into one comma-separated value.
	CommaQuery bool
	// Err is the error no error handler turned into a response.
	Err error
}

type invocationKey struct{}

// NewContext returns ctx carrying inv.
func NewContext(ctx context.Context, inv *Invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, inv)
}

// FromContext returns the invocation of ctx, or nil outside Lambda.
func FromContext(ctx context.Context) *Invocation {
	if inv, ok := ctx.Value(invocationKey{}).(*Invocation); ok {
		return inv
	}
	return nil
}

// ResponseBuffer is an http.ResponseWriter that keeps the whole response in
// memory.
type ResponseBuffer struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

// NewResponseBuffer returns an empty buffer.
func NewResponseBuffer() *ResponseBuffer {
	return &ResponseBuffer{header: http.Header{}}
}

// Header returns the response headers.
func (b *ResponseBuffer) Header() http.Header {
	return b.header
}

// WriteHeader records the status code; later calls are ignored, as net/http
// ignores them.
func (b *ResponseBuffer) WriteHeader(status int) {
	if b.wroteHeader {
		return
	}
	b.status = status
	b.wroteHeader = true
}

// Write appends p to the body, sending the 200 status first when none was
// written.
func (b *ResponseBuffer) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	// A bytes.Buffer grows or panics; it never returns an error.
	n, _ := b.body.Write(p)
	return n, nil
}

// StatusCode returns the status code, 200 when the handler wrote none.
func (b *ResponseBuffer) StatusCode() int {
	if !b.wroteHeader {
		return http.StatusOK
	}
	return b.status
}

// Body returns the body written so far.
func (b *ResponseBuffer) Body() []byte {
	return b.body.Bytes()
}

// SetBody replaces the body.
func (b *ResponseBuffer) SetBody(body []byte) {
	b.body.Reset()
	b.body.Write(body)
}

// SetStatus replaces the status code.
func (b *ResponseBuffer) SetStatus(status int) {
	b.status = status
	b.wroteHeader = true
}

// Reset discards the status and the body written, and the headers that
// describe the body, so that an error response replaces a partial one. The
// other headers stay: a middleware may have set them before calling the
// handler.
func (b *ResponseBuffer) Reset() {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding"} {
		b.header.Del(name)
	}
	b.status = 0
	b.wroteHeader = false
	b.body.Reset()
}

// CopyTo copies the response to w.
func (b *ResponseBuffer) CopyTo(w http.ResponseWriter) error {
	maps.Copy(w.Header(), b.header)
	w.WriteHeader(b.StatusCode())
	if _, err := w.Write(b.body.Bytes()); err != nil {
		return fmt.Errorf("write the response: %w", err)
	}
	return nil
}

// Serve runs h on req, which carries inv in its context, and returns the
// buffered response and the error h left unhandled.
func Serve(h http.Handler, req *http.Request, inv *Invocation) (*ResponseBuffer, error) {
	buf := NewResponseBuffer()
	h.ServeHTTP(buf, req)
	if buf.Header().Get("Content-Type") == "" && buf.body.Len() > 0 {
		// net/http sniffs the type of a body written without one; the proxy
		// response must carry what a server would have sent.
		buf.Header().Set("Content-Type", http.DetectContentType(buf.body.Bytes()))
	}
	return buf, inv.Err
}
