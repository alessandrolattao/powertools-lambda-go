package oauth2

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// api is a downstream API on TLS that records the Authorization header it
// receives and answers with status.
type api struct {
	server *httptest.Server
	status atomic.Int32
	delay  atomic.Int64
	seen   atomic.Value
	hits   atomic.Int32
}

func newAPI(t *testing.T) *api {
	t.Helper()
	a := &api{}
	a.status.Store(http.StatusOK)
	a.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		a.seen.Store(r.Method + " " + r.Header.Get("Authorization"))
		if d := time.Duration(a.delay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(int(a.status.Load()))
		_, _ = io.WriteString(w, `{"stock":3}`) //nolint:errcheck // a test server
	}))
	// httptest servers share one certificate: the token endpoint's client
	// trusts this server too.
	a.server.StartTLS()
	t.Cleanup(a.server.Close)
	return a
}

func TestHTTPClient(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
	a := newAPI(t)
	client := e.newClient(t).HTTPClient()

	for _, status := range []int32{http.StatusOK, http.StatusUnauthorized} {
		a.status.Store(status)
		// A lower-case method is sent upper-cased, as Powertools does.
		req, err := http.NewRequestWithContext(t.Context(), strings.ToLower(http.MethodGet), a.server.URL+"/stock/item%2F123", http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, int(status), resp.StatusCode)
		assert.JSONEq(t, `{"stock":3}`, string(body))
		assert.Equal(t, "GET Bearer tok-1", a.seen.Load(), "the method is upper-cased")
	}
	assert.Equal(t, int32(1), e.hits.Load(), "a 401 does not discard the cached token")
	assert.Equal(t, int32(2), a.hits.Load(), "nothing is replayed")
}

func TestTransportRefusesRequests(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
	client := e.newClient(t).HTTPClient()
	build := func(method, target string, header http.Header) *http.Request {
		req, err := http.NewRequestWithContext(t.Context(), method, target, http.NoBody)
		require.NoError(t, err)
		maps.Copy(req.Header, header)
		return req
	}
	cases := map[string]*http.Request{
		"http":                     build(http.MethodGet, "http://api.example.com/", nil),
		"user information":         build(http.MethodGet, "https://u:p@api.example.com/", nil),
		"fragment":                 build(http.MethodGet, "https://api.example.com/#x", nil),
		"method with a dash":       build("M-SEARCH", "https://api.example.com/", nil),
		"own Authorization":        build(http.MethodGet, "https://api.example.com/", http.Header{"Authorization": {"Basic x"}}),
		"lower-case authorization": build(http.MethodGet, "https://api.example.com/", http.Header{"authorization": {"Bearer x"}}),
		"header name with space":   build(http.MethodGet, "https://api.example.com/", http.Header{"X Bad": {"v"}}),
		"header value with LF":     build(http.MethodGet, "https://api.example.com/", http.Header{"X-Bad": {"a\nb"}}),
		"header value with DEL":    build(http.MethodGet, "https://api.example.com/", http.Header{"X-Bad": {"a\x7f"}}),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resp, err := client.Do(req)
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			require.ErrorIs(t, err, ErrInvalidRequest)
		})
	}
	assert.Equal(t, int32(0), e.hits.Load(), "no token is acquired for a refused request")

	t.Run("accepted headers", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validRequest(build(http.MethodPost, "https://api.example.com/?q=1", http.Header{
			"X-Tab": {"a\tb"}, "X-Latin": {"café"}, "X-Token-Chars": {"!#$%&'*+-.^_`|~"},
		})))
	})
}

func TestTransportFailures(t *testing.T) {
	t.Parallel()

	t.Run("token exchange", func(t *testing.T) {
		t.Parallel()
		e := newTokenEndpoint(t, reply{http.StatusBadRequest, "", ""})
		a := newAPI(t)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL, http.NoBody)
		require.NoError(t, err)
		resp, err := e.newClient(t).HTTPClient().Do(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.ErrorIs(t, err, ErrTokenExchange)
		assert.Equal(t, int32(0), a.hits.Load())
	})

	t.Run("downstream transport", func(t *testing.T) {
		t.Parallel()
		e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
		a := newAPI(t)
		target := a.server.URL
		a.server.Close()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader("{}"))
		require.NoError(t, err)
		resp, err := e.newClient(t).HTTPClient().Do(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.ErrorIs(t, err, ErrDownstreamRequest)
	})

	t.Run("request timeout", func(t *testing.T) {
		t.Parallel()
		e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
		a := newAPI(t)
		a.delay.Store(int64(10 * time.Second))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL, http.NoBody)
		require.NoError(t, err)
		start := time.Now()
		resp, err := e.newClient(t, WithRequestTimeout(100*time.Millisecond)).HTTPClient().Do(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.ErrorIs(t, err, ErrDownstreamRequest)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}

func TestTransportOverACustomBase(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
	c := e.newClient(t)
	var seen string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = req.Header.Get("Authorization")
		_, hasDeadline := req.Context().Deadline()
		assert.True(t, hasDeadline, "the request timeout applies")
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), "", "https://api.example.com/", http.NoBody)
	require.NoError(t, err)
	resp, err := c.Transport(base).RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer tok-1", seen)
	assert.Empty(t, req.Header.Get("Authorization"), "the caller's request is not modified")
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// closeTracker records whether a request body was closed.
type closeTracker struct {
	io.Reader

	closed atomic.Bool
}

func (c *closeTracker) Close() error {
	c.closed.Store(true)
	return nil
}

func TestTransportClosesRefusedBodies(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, reply{http.StatusBadRequest, "", ""})
	c := e.newClient(t)
	for _, target := range []string{"http://api.example.com/", "https://api.example.com/"} {
		body := &closeTracker{Reader: strings.NewReader("{}")}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, body)
		require.NoError(t, err)
		resp, err := c.Transport(nil).RoundTrip(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.Error(t, err)
		assert.True(t, body.closed.Load(), target)
	}
}
