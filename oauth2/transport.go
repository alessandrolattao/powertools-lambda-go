package oauth2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Transport returns a RoundTripper that sends requests through base (the
// client's transport when nil) with an access token in their Authorization
// header: the Go side of Powertools' request(). Before acquiring a token it
// refuses, with ErrInvalidRequest, a request that is not HTTPS, has a
// method that is not letters only, an invalid header, or an Authorization
// header of its own. Each request then has WithRequestTimeout to complete,
// until its response body is closed.
//
// Responses come back whatever their status: a 401 does not discard the
// cached token, and nothing is replayed. A transport failure matches
// ErrDownstreamRequest. Send it only to the resource the token is for: the
// destination is not checked against the audience or the resource. An
// http.Client built around it must refuse redirects (CheckRedirect
// returning http.ErrUseLastResponse), or a redirect would carry the token
// to another host; HTTPClient does.
func (c *Client) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = c.transport
	}
	return &transport{client: c, base: base}
}

// HTTPClient returns an *http.Client that sends its requests through
// Transport and follows no redirect.
func (c *Client) HTTPClient() *http.Client {
	return &http.Client{Transport: c.Transport(nil), CheckRedirect: noRedirect}
}

type transport struct {
	client *Client
	base   http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validRequest(req); err != nil {
		closeRequestBody(req)
		return nil, err
	}
	header, err := t.client.AuthHeaders(req.Context())
	if err != nil {
		closeRequestBody(req)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(req.Context(), t.client.requestTimeout)
	out := req.Clone(ctx)
	out.Method = strings.ToUpper(req.Method)
	if out.Method == "" {
		out.Method = http.MethodGet
	}
	out.Header.Set("Authorization", header.Get("Authorization"))
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		cancel()
		debug.Logger().DebugContext(req.Context(), "oauth2: authenticated request failed", "err", err)
		return nil, errDownstream()
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// validRequest applies Powertools' checks to a request before any secret
// or token is involved.
func validRequest(req *http.Request) error {
	if req.URL == nil || !authval.HTTPSURL(req.URL.String(), false) {
		return fmt.Errorf("%w: an HTTPS URL without user information or a fragment is required", ErrInvalidRequest)
	}
	for i := range len(req.Method) {
		if !isLetter(req.Method[i]) {
			return fmt.Errorf("%w: the method must be letters only", ErrInvalidRequest)
		}
	}
	for name, values := range req.Header {
		if strings.EqualFold(name, "Authorization") || !validHeaderName(name) {
			return fmt.Errorf("%w: headers must be valid and must not include Authorization", ErrInvalidRequest)
		}
		for _, value := range values {
			if !validHeaderValue(value) {
				return fmt.Errorf("%w: headers must be valid and must not include Authorization", ErrInvalidRequest)
			}
		}
	}
	return nil
}

// validHeaderName reports an HTTP token, RFC 9110's field name syntax.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		ch := name[i]
		if !isLetter(ch) && !isDigit(ch) && !strings.ContainsRune("-!#$%&'*+.^_`|~", rune(ch)) {
			return false
		}
	}
	return true
}

// validHeaderValue accepts tabs, visible ASCII and bytes of extended
// Latin-1 or UTF-8: no control characters.
func validHeaderValue(value string) bool {
	for i := range len(value) {
		if ch := value[i]; (ch < 0x20 && ch != '\t') || ch == 0x7f {
			return false
		}
	}
	return true
}

// closeRequestBody honours the RoundTripper contract on the paths that
// never reach the base transport.
func closeRequestBody(req *http.Request) {
	if req.Body == nil {
		return
	}
	if err := req.Body.Close(); err != nil {
		debug.Logger().DebugContext(req.Context(), "oauth2: closing a request body failed", "err", err)
	}
}

// cancelOnClose ends a request's timeout when its response body is closed.
type cancelOnClose struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close() //nolint:wrapcheck // a body's Close error is the transport's own
}
