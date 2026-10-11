package auth

import (
	"maps"
	"net/http"
	"slices"
	"time"
)

// The defaults of Powertools Python's JWTVerifier.
const (
	DefaultClockSkew          = 60 * time.Second
	DefaultTimeout            = 3 * time.Second
	DefaultJWKSMaxAge         = 300 * time.Second
	DefaultUnknownKIDCooldown = 300 * time.Second
)

// Option configures a JWTVerifier.
type Option func(*config)

type config struct {
	jwks            []byte
	jwksSet         bool
	jwksURI         string
	jwksURISet      bool
	requiredClaims  []string
	expectedClaims  map[string]string
	expectedHeaders map[string]string
	clockSkew       time.Duration
	timeout         time.Duration
	maxAge          time.Duration
	cooldown        time.Duration
	client          *http.Client
	now             func() time.Time
}

func newConfig(opts []Option) config {
	cfg := config{
		clockSkew: DefaultClockSkew,
		timeout:   DefaultTimeout,
		maxAge:    DefaultJWKSMaxAge,
		cooldown:  DefaultUnknownKIDCooldown,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithJWKS verifies with a static JSON Web Key Set instead of fetching one:
// no network is involved, and rotating the keys is up to the application,
// which builds a new verifier when they change. It excludes WithJWKSURI.
func WithJWKS(jwks []byte) Option {
	return func(c *config) {
		c.jwks = slices.Clone(jwks)
		c.jwksSet = true
	}
}

// WithJWKSURI fetches the keys from uri, an HTTPS URL, instead of
// discovering it from the issuer's OpenID configuration. It excludes
// WithJWKS.
func WithJWKSURI(uri string) Option {
	return func(c *config) {
		c.jwksURI = uri
		c.jwksURISet = true
	}
}

// WithRequiredClaims requires claims besides iss, aud and exp, which are
// always required. A claim that is null counts as missing.
func WithRequiredClaims(names ...string) Option {
	return func(c *config) { c.requiredClaims = append(c.requiredClaims, names...) }
}

// WithExpectedClaims requires string claims with exactly these values, such
// as {"token_use": "access"}, so that a token of another kind with the same
// audience is refused. A missing claim fails the check.
func WithExpectedClaims(claims map[string]string) Option {
	return func(c *config) { c.expectedClaims = maps.Clone(claims) }
}

// WithExpectedHeaders requires JOSE header parameters with exactly these
// values, such as {"typ": "at+jwt"}.
func WithExpectedHeaders(headers map[string]string) Option {
	return func(c *config) { c.expectedHeaders = maps.Clone(headers) }
}

// WithClockSkew sets the tolerance applied to exp, nbf and iat. The default
// is one minute; it must not be negative.
func WithClockSkew(d time.Duration) Option {
	return func(c *config) { c.clockSkew = d }
}

// WithTimeout bounds the discovery and key requests of one verification,
// and the wait for another caller's refresh. The default is three seconds:
// keep it below the function's timeout, so that the function has time to
// answer when the identity provider does not.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithJWKSMaxAge sets how long fetched keys are trusted. Past it they are
// refreshed, and never used again if the refresh fails. The default is five
// minutes.
func WithJWKSMaxAge(d time.Duration) Option {
	return func(c *config) { c.maxAge = d }
}

// WithUnknownKIDCooldown sets the minimum interval between two refreshes
// caused by a token whose kid is not in the key set, so that forged tokens
// cannot hammer the identity provider. The default is five minutes; zero
// refreshes on every unknown kid.
func WithUnknownKIDCooldown(d time.Duration) Option {
	return func(c *config) { c.cooldown = d }
}

// WithHTTPClient sends the discovery and key requests through client, for
// a proxy, instrumentation or a private certificate authority. Redirects are
// never followed, whatever the client says.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) { c.client = client }
}

// withClock replaces the clock, for tests. A verifier with its own clock
// keeps its keys to itself.
func withClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}
