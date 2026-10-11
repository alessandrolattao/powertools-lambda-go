package oauth2

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// reply is one answer of the token endpoint.
type reply struct {
	status      int
	contentType string
	body        string
}

func tokenReply(token, expiresIn string) reply {
	body := `{"access_token":"` + token + `","token_type":"Bearer"`
	if expiresIn != "" {
		body += `,"expires_in":` + expiresIn
	}
	return reply{status: http.StatusOK, contentType: "application/json", body: body + "}"}
}

// seen is what the token endpoint received.
type seen struct {
	authorization string
	contentType   string
	form          url.Values
}

// tokenEndpoint is a token endpoint on TLS whose answers a test queues;
// the last answer repeats.
type tokenEndpoint struct {
	server *httptest.Server
	hits   atomic.Int32

	mu      sync.Mutex
	replies []reply
	seen    []seen
	gate    chan struct{}
	before  func()
}

func newTokenEndpoint(t *testing.T, replies ...reply) *tokenEndpoint {
	t.Helper()
	e := &tokenEndpoint{replies: replies}
	e.server = httptest.NewUnstartedServer(http.HandlerFunc(e.serve))
	e.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	e.server.StartTLS()
	t.Cleanup(e.server.Close)
	return e
}

func (e *tokenEndpoint) url() string { return e.server.URL + "/oauth2/token" }

func (e *tokenEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	e.hits.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	e.seen = append(e.seen, seen{authorization: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), form: r.PostForm})
	answer := e.replies[0]
	if len(e.replies) > 1 {
		e.replies = e.replies[1:]
	}
	gate, before := e.gate, e.before
	e.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if before != nil {
		before()
	}
	if answer.contentType != "" {
		w.Header().Set("Content-Type", answer.contentType)
	}
	w.WriteHeader(answer.status)
	_, _ = io.WriteString(w, answer.body) //nolint:errcheck // a test server
}

func (e *tokenEndpoint) set(update func(e *tokenEndpoint)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	update(e)
}

func (e *tokenEndpoint) requests() []seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]seen(nil), e.seen...)
}

// newClient is a client of the endpoint with the secret "s3cret".
func (e *tokenEndpoint) newClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	c, err := New(e.url(), "orders", StaticSecret("s3cret"), append([]Option{WithHTTPClient(e.server.Client())}, opts...)...)
	require.NoError(t, err)
	return c
}

// clock is a settable clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_800_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// requireExchangeError checks a token exchange failure and its retryable
// flag.
func requireExchangeError(t *testing.T, err error, retryable bool) {
	t.Helper()
	require.ErrorIs(t, err, ErrTokenExchange)
	e, ok := errors.AsType[*Error](err)
	require.True(t, ok)
	require.Equal(t, retryable, e.Retryable, "retryable")
}

// countingSecret counts the lookups of a static secret.
func countingSecret(secret string, calls *atomic.Int32) SecretSource {
	return func(context.Context) (string, error) {
		calls.Add(1)
		return secret, nil
	}
}
