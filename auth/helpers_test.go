package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jws/jwsbb"
	"github.com/stretchr/testify/require"
)

// The issuer and audience most tests use with static keys.
const (
	testIssuer   = "https://idp.example.com/"
	testAudience = "https://orders.example.com"
)

// rsaKeys are generated once: 2048-bit generation is slow enough to matter
// across hundreds of tests.
var rsaKeys = sync.OnceValue(func() [2]*rsa.PrivateKey {
	var keys [2]*rsa.PrivateKey
	for i := range keys {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keys[i] = key
	}
	return keys
})

// testKey is a signing key and the JWK that publishes it.
type testKey struct {
	alg    Algorithm
	kid    string
	signer any
}

// newKey makes a key for alg. The RSA algorithms share the first cached
// RSA key; use rotatedRSA for a second one.
func newKey(tb testing.TB, alg Algorithm, kid string) *testKey {
	tb.Helper()
	var signer any
	var err error
	switch alg {
	case RS256, RS384, RS512, PS256, PS384, PS512:
		signer = rsaKeys()[0]
	case ES256:
		signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case ES384:
		signer, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case ES512:
		signer, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	case ES256K:
		var key *secp256k1.PrivateKey
		key, err = secp256k1.GeneratePrivateKey()
		if err == nil {
			signer = key.ToECDSA()
		}
	case EdDSA:
		_, signer, err = ed25519.GenerateKey(rand.Reader)
	default:
		tb.Fatalf("no key for %s", alg)
	}
	require.NoError(tb, err)
	return &testKey{alg: alg, kid: kid, signer: signer}
}

// rotatedRSA is an RS256 key distinct from the one newKey uses.
func rotatedRSA(kid string) *testKey {
	return &testKey{alg: RS256, kid: kid, signer: rsaKeys()[1]}
}

// jwk returns the public JWK of the key, with its kid and any extra member;
// a nil extra value removes the member.
func (k *testKey) jwk(tb testing.TB, extra map[string]any) map[string]any {
	tb.Helper()
	key, err := jwk.Import[jwk.Key](k.signer)
	require.NoError(tb, err)
	public, err := key.PublicKey()
	require.NoError(tb, err)
	raw, err := json.Marshal(public)
	require.NoError(tb, err)
	var out map[string]any
	require.NoError(tb, json.Unmarshal(raw, &out))
	out["kid"] = k.kid
	for name, value := range extra {
		if value == nil {
			delete(out, name)
		} else {
			out[name] = value
		}
	}
	return out
}

// jwksOf is the JWKS document of keys.
func jwksOf(tb testing.TB, keys ...map[string]any) []byte {
	tb.Helper()
	raw, err := json.Marshal(map[string]any{"keys": keys})
	require.NoError(tb, err)
	return raw
}

// claimsAt is a valid claims set for testIssuer and testAudience at now.
func claimsAt(now time.Time) map[string]any {
	return map[string]any{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "user-1",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// with returns a copy of claims with changes applied; a nil value removes
// the claim.
func with(claims, changes map[string]any) map[string]any {
	out := maps.Clone(claims)
	for name, value := range changes {
		if value == nil {
			delete(out, name)
		} else {
			out[name] = value
		}
	}
	return out
}

// sign signs claims with the key, under a header with its alg and kid plus
// any extra member; a nil extra value removes the member.
func (k *testKey) sign(tb testing.TB, claims any, extra map[string]any) string {
	tb.Helper()
	header := map[string]any{"alg": string(k.alg), "kid": k.kid, "typ": "JWT"}
	for name, value := range extra {
		if value == nil {
			delete(header, name)
		} else {
			header[name] = value
		}
	}
	headerJSON, err := json.Marshal(header, json.Deterministic(true))
	require.NoError(tb, err)
	payload, ok := claims.([]byte)
	if !ok {
		payload, err = json.Marshal(claims, json.Deterministic(true))
		require.NoError(tb, err)
	}
	return k.signRaw(tb, headerJSON, payload)
}

// signRaw signs arbitrary header and payload bytes.
func (k *testKey) signRaw(tb testing.TB, header, payload []byte) string {
	tb.Helper()
	input := b64(header) + "." + b64(payload)
	signature, err := jwsbb.Sign(k.signer, string(k.alg), []byte(input), nil)
	require.NoError(tb, err)
	return input + "." + b64(signature)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

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

// staticVerifier verifies with a static key set at the clock's time.
func staticVerifier(tb testing.TB, c *clock, jwks []byte, opts ...Option) *JWTVerifier {
	tb.Helper()
	opts = append([]Option{WithJWKS(jwks), withClock(c.Now)}, opts...)
	v, err := NewJWTVerifier(testIssuer, []string{testAudience}, asymmetric, opts...)
	require.NoError(tb, err)
	return v
}

// requireReason checks that err is an *Error with the reason.
func requireReason(t *testing.T, err error, reason Reason) {
	t.Helper()
	authErr, ok := errors.AsType[*Error](err)
	require.True(t, ok, "want an *auth.Error, got %v", err)
	require.Equal(t, reason, authErr.Reason)
}

// idp is an identity provider on TLS: an OpenID configuration and a JWKS,
// both of which a test can change at any time.
type idp struct {
	server *httptest.Server

	mu            sync.Mutex
	jwks          []byte
	jwksStatus    int
	jwksDelay     time.Duration
	jwksGate      chan struct{}
	discoveryBody map[string]any

	discoveryHits atomic.Int32
	jwksHits      atomic.Int32
}

func newIDP(t *testing.T, jwks []byte) *idp {
	t.Helper()
	p := &idp{jwks: jwks, jwksStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", p.keys)
	p.server = httptest.NewUnstartedServer(mux)
	// The certificate tests make handshakes fail on purpose.
	p.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	p.server.StartTLS()
	t.Cleanup(p.server.Close)
	p.discoveryBody = map[string]any{"issuer": p.issuer(), "jwks_uri": p.server.URL + "/keys"}
	return p
}

// issuer is the provider's issuer, with the trailing slash many providers
// use.
func (p *idp) issuer() string { return p.server.URL + "/" }

func (p *idp) discovery(w http.ResponseWriter, _ *http.Request) {
	p.discoveryHits.Add(1)
	p.mu.Lock()
	body, err := json.Marshal(p.discoveryBody)
	p.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) //nolint:errcheck // a test server
}

func (p *idp) keys(w http.ResponseWriter, r *http.Request) {
	p.jwksHits.Add(1)
	p.mu.Lock()
	body, status, delay, gate := p.jwks, p.jwksStatus, p.jwksDelay, p.jwksGate
	p.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body) //nolint:errcheck // a test server
}

func (p *idp) set(update func(p *idp)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	update(p)
}

// verifier verifies the provider's tokens through discovery, with its own
// cache unless shared is set.
func (p *idp) verifier(t *testing.T, c *clock, opts ...Option) *JWTVerifier {
	t.Helper()
	base := []Option{WithHTTPClient(p.server.Client())}
	if c != nil {
		base = append(base, withClock(c.Now))
	}
	v, err := NewJWTVerifier(p.issuer(), []string{testAudience}, asymmetric, append(base, opts...)...)
	require.NoError(t, err)
	return v
}

// claims is a valid claims set for the provider at now.
func (p *idp) claims(now time.Time) map[string]any {
	return with(claimsAt(now), map[string]any{"iss": p.issuer()})
}

// jsonNull marshals to null: a claim or member present with a null value,
// where a nil map value removes it.
type jsonNull struct{}

func (jsonNull) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// jsonRaw marshals to itself.
type jsonRaw string

func (r jsonRaw) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// privateJWK publishes the private half of a key too, as a careless provider
// might.
func privateJWK(tb testing.TB, k *testKey) map[string]any {
	tb.Helper()
	key, err := jwk.Import[jwk.Key](k.signer)
	require.NoError(tb, err)
	raw, err := json.Marshal(key)
	require.NoError(tb, err)
	var out map[string]any
	require.NoError(tb, json.Unmarshal(raw, &out))
	require.Contains(tb, out, "d")
	out["kid"] = k.kid
	return out
}
