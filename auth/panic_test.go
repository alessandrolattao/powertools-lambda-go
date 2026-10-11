package auth

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errTransportBug = errors.New("transport bug")

// panicOnce is a transport of the caller's that panics on its first request.
type panicOnce struct {
	base  http.RoundTripper
	calls atomic.Int32
}

func (p *panicOnce) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.calls.Add(1) == 1 {
		panic(errTransportBug)
	}
	return p.base.RoundTrip(r) //nolint:wrapcheck // a transparent test transport
}

// A transport that panics during a refresh must not leave the verifiers
// waiting on a refresh that will never end.
func TestAPanickingTransportDoesNotWedgeTheKeyCache(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	client := p.server.Client()
	client.Transport = &panicOnce{base: client.Transport}
	v := p.verifier(t, c, WithHTTPClient(client))
	token := key.sign(t, p.claims(c.Now()), nil)

	assert.PanicsWithValue(t, errTransportBug, func() {
		_, _ = v.Verify(t.Context(), token) //nolint:errcheck // it panics
	})

	start := time.Now()
	_, err := v.Verify(t.Context(), token)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second, "the next verification does not wait for a timeout")
}
