package eventhandler

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Content types the event handler writes.
const (
	ContentTypeJSON = "application/json"
	ContentTypeText = "text/plain"
	ContentTypeHTML = "text/html"
)

// Response is what a typed operation returns when the status, the headers or
// the cookies are its own to choose: Python's Response. Body is written as
// JSON (unless T is []byte, written as it is) with Content-Type
// application/json unless Header sets another; the OpenAPI document and
// response validation describe T.
type Response[T any] struct {
	// StatusCode is the status; zero means the route's (see StatusCode).
	StatusCode int
	// Header holds response headers.
	Header http.Header
	// Cookies are sent with Set-Cookie. Unlike Python's Cookie, Go's
	// http.Cookie is not Secure unless asked.
	Cookies []*http.Cookie
	// Body is the response body.
	Body T
	// Compress, when set, decides the compression of this response over the
	// route's Compress option (the client must still accept gzip).
	Compress *bool
}

// responder is implemented by every Response.
type responder interface {
	parts() responseParts
	bodyType() reflect.Type
}

// responseParts is a Response whatever its body type.
type responseParts struct {
	status   int
	header   http.Header
	cookies  []*http.Cookie
	body     any
	compress *bool
}

func (r Response[T]) parts() responseParts {
	return responseParts{status: r.StatusCode, header: r.Header, cookies: r.Cookies, body: r.Body, compress: r.Compress}
}

func (Response[T]) bodyType() reflect.Type { return reflect.TypeFor[T]() }

// WriteJSON writes v as a JSON response with status, encoded with the
// serializer of the router serving r (see WithSerializer), the way the
// router writes its own JSON responses.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) error {
	body, err := serialize(r, v)
	if err != nil {
		return err
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", ContentTypeJSON)
	}
	w.WriteHeader(status)
	// G705: the body is JSON served as application/json, not markup.
	if _, err := w.Write(body); err != nil { //nolint:gosec // see above
		return fmt.Errorf("write the response: %w", err)
	}
	return nil
}

// serialize encodes v with the serializer of the router serving r.
func serialize(r *http.Request, v any) ([]byte, error) {
	var body []byte
	var err error
	if st := stateFrom(r.Context()); st != nil {
		body, err = st.router.cfg.serializer(v)
	} else {
		body, err = marshalJSON(v)
	}
	if err != nil {
		return nil, fmt.Errorf("encode the response as JSON: %w", err)
	}
	return body, nil
}

// finish applies what the route asks of every response, after the handler
// and the middlewares: CORS headers, Cache-Control, compression.
func (r *Router) finish(buf *lambdahttp.ResponseBuffer, req *http.Request, st *requestState) {
	rt := st.route
	h := buf.Header()
	if h.Get("Content-Type") == "" && len(buf.Body()) > 0 {
		// net/http sniffs the type of a body written without one.
		h.Set("Content-Type", http.DetectContentType(buf.Body()))
	}
	if r.corsEnabled(rt) {
		cfg := r.cfg.cors
		if cfg == nil {
			cfg = &CORSConfig{}
		}
		cfg.apply(h, req.Header.Get("Origin"))
	}
	if rt != nil && rt.cfg.cacheControl != "" {
		value := rt.cfg.cacheControl
		if buf.StatusCode() != http.StatusOK {
			value = "no-cache"
		}
		h.Set("Cache-Control", value)
	}
	compress := rt != nil && rt.cfg.compress
	if st.compress != nil {
		compress = *st.compress
	}
	if compress && len(buf.Body()) > 0 {
		// The body depends on Accept-Encoding: caches must keep one per
		// encoding.
		addVary(h, "Accept-Encoding")
		if acceptsGzip(req) {
			r.compress(buf, req)
		}
	}
}

// acceptsGzip mirrors Python's check: "gzip" anywhere in Accept-Encoding.
func acceptsGzip(req *http.Request) bool {
	return strings.Contains(strings.Join(req.Header.Values("Accept-Encoding"), ","), "gzip")
}

func (r *Router) compress(buf *lambdahttp.ResponseBuffer, req *http.Request) {
	body := buf.Body()
	h := buf.Header()
	// A response without a body (204, 304) has nothing to compress, and a
	// body the handler encoded already must not be encoded twice.
	if len(body) == 0 || h.Get("Content-Encoding") != "" {
		return
	}
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err == nil {
		_, err = zw.Write(body)
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		debug.Logger().DebugContext(req.Context(), "event handler: compression failed, sending the body as it is", "err", err)
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	buf.SetBody(out.Bytes())
}
