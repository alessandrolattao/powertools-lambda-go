package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryAndCaching(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c)

	for range 3 {
		_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), p.discoveryHits.Load())
	assert.Equal(t, int32(1), p.jwksHits.Load(), "warm verifications reuse the keys")
}

func TestJWKSURISkipsDiscovery(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, EdDSA, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c, WithJWKSURI(p.server.URL+"/keys"))

	_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)
	assert.Equal(t, int32(0), p.discoveryHits.Load())
	assert.Equal(t, int32(1), p.jwksHits.Load())
}

func TestDiscoveryFailures(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	cases := map[string]map[string]any{
		"issuer mismatch":       {"issuer": "https://evil.example.com/"},
		"issuer without slash":  {"issuer": "TRIM"},
		"issuer missing":        {"issuer": nil},
		"jwks_uri missing":      {"jwks_uri": nil},
		"jwks_uri over http":    {"jwks_uri": "http://127.0.0.1/keys"},
		"jwks_uri not a string": {"jwks_uri": 1},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
			p.set(func(p *idp) {
				for member, value := range change {
					switch value {
					case nil:
						delete(p.discoveryBody, member)
					case "TRIM":
						p.discoveryBody[member] = strings.TrimSuffix(p.issuer(), "/")
					default:
						p.discoveryBody[member] = value
					}
				}
			})
			_, err := p.verifier(t, c).Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
			requireReason(t, err, ReasonJWKSUnavailable)
			require.ErrorIs(t, err, ErrJWKSFetch)
			assert.Equal(t, int32(0), p.jwksHits.Load())
		})
	}
}

func TestJWKSResponseFailures(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	cases := map[string]func(p *idp){
		"status 500":       func(p *idp) { p.jwksStatus = http.StatusInternalServerError },
		"status 204":       func(p *idp) { p.jwksStatus = http.StatusNoContent },
		"not JSON":         func(p *idp) { p.jwks = []byte(`<html>`) },
		"JSON array":       func(p *idp) { p.jwks = []byte(`[]`) },
		"JSON null":        func(p *idp) { p.jwks = []byte(`null`) },
		"keys not a list":  func(p *idp) { p.jwks = []byte(`{"keys":"x"}`) },
		"key not object":   func(p *idp) { p.jwks = []byte(`{"keys":["x"]}`) },
		"larger than 1MiB": func(p *idp) { p.jwks = []byte(`{"keys":[],"pad":"` + strings.Repeat("x", 1<<20) + `"}`) },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
			p.set(breakIt)
			_, err := p.verifier(t, c).Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
			requireReason(t, err, ReasonJWKSUnavailable)
		})
	}

	t.Run("redirects are not followed", func(t *testing.T) {
		t.Parallel()
		p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
		redirect := httptest.NewTLSServer(http.RedirectHandler(p.server.URL+"/keys", http.StatusFound))
		t.Cleanup(redirect.Close)
		v := p.verifier(t, c, WithJWKSURI(redirect.URL+"/keys"))
		_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
		requireReason(t, err, ReasonJWKSUnavailable)
		assert.Equal(t, int32(0), p.jwksHits.Load())
	})

	t.Run("untrusted certificate", func(t *testing.T) {
		t.Parallel()
		p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
		v, err := NewJWTVerifier(p.issuer(), []string{testAudience}, []Algorithm{ES256}, withClock(c.Now))
		require.NoError(t, err)
		_, err = v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
		requireReason(t, err, ReasonJWKSUnavailable)
	})
}

func TestUnknownKIDRefresh(t *testing.T) {
	t.Parallel()
	c := newClock()
	old := newKey(t, ES256, "old")
	rotated := newKey(t, ES256, "new")
	p := newIDP(t, jwksOf(t, old.jwk(t, nil)))
	v := p.verifier(t, c)

	_, err := v.Verify(t.Context(), old.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)

	// The provider rotates its key.
	p.set(func(p *idp) { p.jwks = jwksOf(t, rotated.jwk(t, nil)) })

	// Within the cooldown, an unknown kid does not refresh: forged tokens
	// cannot hammer the provider.
	_, err = v.Verify(t.Context(), rotated.sign(t, p.claims(c.Now()), nil))
	requireReason(t, err, ReasonInvalidToken)
	assert.Equal(t, int32(1), p.jwksHits.Load())

	// Past the cooldown, it does, and the new key verifies.
	c.Advance(DefaultUnknownKIDCooldown)
	_, err = v.Verify(t.Context(), rotated.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)
	assert.Equal(t, int32(2), p.jwksHits.Load())

	// The refresh replaced the set: the removed key is gone.
	_, err = v.Verify(t.Context(), old.sign(t, p.claims(c.Now()), nil))
	requireReason(t, err, ReasonInvalidToken)
	assert.Equal(t, int32(2), p.jwksHits.Load(), "the cooldown started again")
}

func TestUnknownKIDWithoutCooldown(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c, WithUnknownKIDCooldown(0))

	stranger := newKey(t, ES256, "stranger")
	for i := range 3 {
		_, err := v.Verify(t.Context(), stranger.sign(t, p.claims(c.Now()), nil))
		requireReason(t, err, ReasonInvalidToken)
		assert.Equal(t, int32(i+1), p.jwksHits.Load())
	}
}

func TestExpiredKeysAreNeverServed(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c, WithJWKSURI(p.server.URL+"/keys"))
	token := func() string { return key.sign(t, p.claims(c.Now()), nil) }

	_, err := v.Verify(t.Context(), token())
	require.NoError(t, err)

	// Within the maximum age, a failing provider does not matter.
	p.set(func(p *idp) { p.jwksStatus = http.StatusServiceUnavailable })
	c.Advance(DefaultJWKSMaxAge - time.Second)
	_, err = v.Verify(t.Context(), token())
	require.NoError(t, err)
	assert.Equal(t, int32(1), p.jwksHits.Load())

	// Past it, the keys expire and the failed refresh is an error: stale
	// keys are never used.
	c.Advance(time.Second)
	_, err = v.Verify(t.Context(), token())
	requireReason(t, err, ReasonJWKSUnavailable)
	require.ErrorIs(t, err, ErrJWKSFetch)
	assert.Equal(t, int32(2), p.jwksHits.Load())

	// The failures back off: 1, 2, 4, 8, 16, then 30 seconds.
	hits := int32(2)
	for _, seconds := range []int{1, 2, 4, 8, 16, 30, 30} {
		backoff := time.Duration(seconds) * time.Second
		c.Advance(backoff - time.Millisecond)
		_, err = v.Verify(t.Context(), token())
		requireReason(t, err, ReasonJWKSUnavailable)
		assert.Equal(t, hits, p.jwksHits.Load(), "no request during the %s backoff", backoff)
		c.Advance(time.Millisecond)
		_, err = v.Verify(t.Context(), token())
		requireReason(t, err, ReasonJWKSUnavailable)
		hits++
		assert.Equal(t, hits, p.jwksHits.Load())
	}

	// A recovered provider resets the backoff.
	p.set(func(p *idp) { p.jwksStatus = http.StatusOK })
	c.Advance(30 * time.Second)
	_, err = v.Verify(t.Context(), token())
	require.NoError(t, err)
	v.cache.mu.Lock()
	assert.Zero(t, v.cache.failures)
	v.cache.mu.Unlock()
}

func TestFailedRefreshKeepsFreshKeys(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c, WithUnknownKIDCooldown(0))

	_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)

	p.set(func(p *idp) { p.jwksStatus = http.StatusBadGateway })
	stranger := newKey(t, ES256, "stranger")
	_, err = v.Verify(t.Context(), stranger.sign(t, p.claims(c.Now()), nil))
	requireReason(t, err, ReasonJWKSUnavailable)

	// The keys are still within their maximum age.
	_, err = v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)
}

func TestConcurrentVerificationsShareOneRefresh(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	gate := make(chan struct{})
	p.set(func(p *idp) { p.jwksGate = gate })
	v := p.verifier(t, c, WithTimeout(10*time.Second))
	token := key.sign(t, p.claims(c.Now()), nil)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range cap(errs) {
		wg.Go(func() {
			_, err := v.Verify(t.Context(), token)
			errs <- err
		})
	}
	require.Eventually(t, func() bool { return p.jwksHits.Load() == 1 }, 5*time.Second, time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), p.jwksHits.Load())
	assert.Equal(t, int32(1), p.discoveryHits.Load())
}

func TestJoinedRefreshDoesNotRepeat(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c, WithUnknownKIDCooldown(0), WithTimeout(10*time.Second))
	_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)

	gate := make(chan struct{})
	p.set(func(p *idp) { p.jwksGate = gate })
	stranger := newKey(t, ES256, "stranger").sign(t, p.claims(c.Now()), nil)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range cap(errs) {
		wg.Go(func() {
			_, err := v.Verify(t.Context(), stranger)
			errs <- err
		})
	}
	require.Eventually(t, func() bool { return p.jwksHits.Load() == 2 }, 5*time.Second, time.Millisecond)
	// Let the waiters queue on the refresh before it ends.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		requireReason(t, err, ReasonInvalidToken)
	}
	assert.Equal(t, int32(2), p.jwksHits.Load(), "callers that joined a refresh do not start another")
}

func TestRefreshTimeout(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	p.set(func(p *idp) { p.jwksDelay = 10 * time.Second })
	v := p.verifier(t, c, WithTimeout(time.Second), WithJWKSURI(p.server.URL+"/keys"))

	start := time.Now()
	_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	requireReason(t, err, ReasonJWKSUnavailable)
	assert.Less(t, time.Since(start), 5*time.Second)

	// A slow provider is a failing one: the next call backs off.
	_, err = v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	requireReason(t, err, ReasonJWKSUnavailable)
	assert.Equal(t, int32(1), p.jwksHits.Load())
}

func TestAbandonedRefreshDoesNotBackOff(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	gate := make(chan struct{})
	p.set(func(p *idp) { p.jwksGate = gate })
	v := p.verifier(t, c, WithTimeout(10*time.Second))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	token := key.sign(t, p.claims(c.Now()), nil)
	go func() {
		_, err := v.Verify(ctx, token)
		done <- err
	}()
	require.Eventually(t, func() bool { return p.jwksHits.Load() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	requireReason(t, <-done, ReasonJWKSUnavailable)

	// The caller went away; the provider is not to blame, and the next
	// caller refreshes at once.
	close(gate)
	_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)
	assert.Equal(t, int32(2), p.jwksHits.Load())
}

func TestPrefetch(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	v := p.verifier(t, c)

	require.NoError(t, v.Prefetch(t.Context()))
	require.NoError(t, v.Prefetch(t.Context()))
	assert.Equal(t, int32(1), p.jwksHits.Load(), "fresh keys are not fetched again")
	_, err := v.Verify(t.Context(), key.sign(t, p.claims(c.Now()), nil))
	require.NoError(t, err)
	assert.Equal(t, int32(1), p.jwksHits.Load())

	broken := newIDP(t, nil)
	broken.set(func(p *idp) { p.jwksStatus = http.StatusInternalServerError })
	err = broken.verifier(t, c).Prefetch(t.Context())
	requireReason(t, err, ReasonJWKSUnavailable)

	g, err := AnyOf(p.verifier(t, c), broken.verifier(t, c))
	require.NoError(t, err)
	requireReason(t, g.Prefetch(t.Context()), ReasonJWKSUnavailable)
}

func TestVerifiersShareTheirCache(t *testing.T) {
	t.Parallel()
	key := newKey(t, ES256, "k1")
	p := newIDP(t, jwksOf(t, key.jwk(t, nil)))
	client := p.server.Client()
	build := func(opts ...Option) *JWTVerifier {
		v, err := NewJWTVerifier(p.issuer(), []string{testAudience}, []Algorithm{ES256}, append([]Option{WithHTTPClient(client)}, opts...)...)
		require.NoError(t, err)
		return v
	}
	first, second := build(), build(WithRequiredClaims("sub"))
	assert.Same(t, first.cache, second.cache, "the same identity shares keys")
	assert.NotSame(t, first.cache, build(WithJWKSMaxAge(time.Minute)).cache, "another maximum age does not")

	now := time.Now()
	_, err := first.Verify(t.Context(), key.sign(t, p.claims(now), nil))
	require.NoError(t, err)
	_, err = second.Verify(t.Context(), key.sign(t, p.claims(now), nil))
	require.NoError(t, err)
	assert.Equal(t, int32(1), p.jwksHits.Load())
}

func TestForgetCache(t *testing.T) {
	t.Parallel()
	id := cacheIdentity{issuer: "https://forget.example.com/"}
	cache := sharedCache(id)
	require.Same(t, cache, sharedCache(id))
	// A live cache keeps its entry.
	forgetCache(id)
	registry.mu.Lock()
	_, ok := registry.caches[id]
	registry.mu.Unlock()
	assert.True(t, ok)
	runtime.KeepAlive(cache)
}
