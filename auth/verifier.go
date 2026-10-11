package auth

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	// ES256K is not part of jwx itself: the extension registers it.
	_ "github.com/jwx-go/es256k/v4"
	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jws"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
)

// Algorithm is a JWS signature algorithm a verifier accepts.
type Algorithm string

// The asymmetric algorithms a verifier can accept. HMAC algorithms and
// "none" are refused: a verifier holds public keys only.
const (
	RS256  Algorithm = "RS256"
	RS384  Algorithm = "RS384"
	RS512  Algorithm = "RS512"
	PS256  Algorithm = "PS256"
	PS384  Algorithm = "PS384"
	PS512  Algorithm = "PS512"
	ES256  Algorithm = "ES256"
	ES384  Algorithm = "ES384"
	ES512  Algorithm = "ES512"
	ES256K Algorithm = "ES256K"
	EdDSA  Algorithm = "EdDSA"
)

var asymmetric = []Algorithm{RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512, ES256K, EdDSA}

// Verifier verifies access tokens. A *JWTVerifier verifies the tokens of
// one issuer, AnyOf routes between several, and the authtest package has
// one for tests. VerifyAuthorizationHeader, VerifyRequest, Require and the
// Authorize functions work with any of them.
type Verifier interface {
	// Verify returns the claims of token, a JWT without the Bearer
	// prefix, once its signature and claims are verified. Failures are
	// *Error values.
	Verify(ctx context.Context, token string) (*Claims, error)
	// Prefetch fetches the signing keys ahead of the first Verify, during
	// the function's initialization.
	Prefetch(ctx context.Context) error
}

// JWTVerifier verifies the JWT access tokens of one issuer. Build it once,
// outside the handler, so that warm invocations reuse its signing keys; it
// is safe for concurrent use.
type JWTVerifier struct {
	issuer          string
	audience        []string
	algorithms      []Algorithm
	static          *keySet
	cache           *jwksCache
	required        []string
	expectedClaims  map[string]string
	expectedHeaders map[string]string
	clockSkew       time.Duration
	timeout         time.Duration
	now             func() time.Time
	cognitoClientID string
}

// NewJWTVerifier verifies the access tokens issuer signs for audience with
// one of algorithms. The issuer is an exact HTTPS URL; the token's iss claim
// must equal it byte for byte, and so must the issuer its OpenID
// configuration announces. At least one audience must be in the token's
// aud claim.
//
// Without WithJWKS or WithJWKSURI, the signing keys come from the issuer's
// OpenID configuration. Configuration errors match ErrInvalidConfig and never
// quote the values they refuse.
func NewJWTVerifier(issuer string, audience []string, algorithms []Algorithm, opts ...Option) (*JWTVerifier, error) {
	return newJWTVerifier(issuer, audience, algorithms, newConfig(opts))
}

func newJWTVerifier(issuer string, audience []string, algorithms []Algorithm, cfg config) (*JWTVerifier, error) {
	if !authval.HTTPSURL(issuer, true) {
		return nil, fmt.Errorf("%w: the issuer must be an HTTPS URL without user information, a query or a fragment", ErrInvalidConfig)
	}
	v := &JWTVerifier{issuer: issuer, clockSkew: cfg.clockSkew, timeout: cfg.timeout, now: cfg.now}
	var ok bool
	if v.audience, ok = authval.Strings(audience); !ok || len(v.audience) == 0 {
		return nil, fmt.Errorf("%w: at least one audience is required, and none may be blank", ErrInvalidConfig)
	}
	if v.algorithms, ok = validAlgorithms(algorithms); !ok {
		return nil, fmt.Errorf("%w: at least one algorithm is required, and only asymmetric ones are supported", ErrInvalidConfig)
	}
	if err := v.setKeys(cfg); err != nil {
		return nil, err
	}
	if err := v.setClaimRules(cfg); err != nil {
		return nil, err
	}
	if v.clockSkew < 0 {
		return nil, fmt.Errorf("%w: the clock skew must not be negative", ErrInvalidConfig)
	}
	if v.now == nil {
		v.now = time.Now
	}
	return v, nil
}

func validAlgorithms(algorithms []Algorithm) ([]Algorithm, bool) {
	out := make([]Algorithm, 0, len(algorithms))
	for _, alg := range algorithms {
		if !slices.Contains(asymmetric, alg) {
			return nil, false
		}
		if !slices.Contains(out, alg) {
			out = append(out, alg)
		}
	}
	return out, len(out) > 0
}

// setKeys configures where the signing keys come from: a static set, or a
// cache shared with the verifiers of the same identity.
func (v *JWTVerifier) setKeys(cfg config) error {
	if cfg.jwksSet && cfg.jwksURISet {
		return fmt.Errorf("%w: a static JWKS and a JWKS URI are mutually exclusive", ErrInvalidConfig)
	}
	if cfg.jwksSet {
		keys, err := parseKeySetJSON(cfg.jwks)
		if err != nil {
			return err
		}
		v.static = keys
	}
	if cfg.jwksURISet && !authval.HTTPSURL(cfg.jwksURI, false) {
		return fmt.Errorf("%w: the JWKS URI must be an HTTPS URL without user information or a fragment", ErrInvalidConfig)
	}
	switch {
	case cfg.timeout <= 0:
		return fmt.Errorf("%w: the timeout must be positive", ErrInvalidConfig)
	case cfg.maxAge <= 0:
		return fmt.Errorf("%w: the JWKS maximum age must be positive", ErrInvalidConfig)
	case cfg.cooldown < 0:
		return fmt.Errorf("%w: the unknown kid cooldown must not be negative", ErrInvalidConfig)
	}
	if v.static != nil {
		return nil
	}
	id := cacheIdentity{issuer: v.issuer, uri: cfg.jwksURI, maxAge: cfg.maxAge, cooldown: cfg.cooldown, client: cfg.client}
	if cfg.now != nil {
		v.cache = newJWKSCache(id, cfg.now)
	} else {
		v.cache = sharedCache(id)
	}
	return nil
}

// setClaimRules configures the claims and headers every token must carry.
func (v *JWTVerifier) setClaimRules(cfg config) error {
	extra, ok := authval.Strings(cfg.requiredClaims)
	if !ok {
		return fmt.Errorf("%w: required claims must not be blank", ErrInvalidConfig)
	}
	required := append([]string{"iss", "aud", "exp"}, extra...)
	slices.Sort(required)
	v.required = slices.Compact(required)
	if !validMapping(cfg.expectedClaims) || !validMapping(cfg.expectedHeaders) {
		return fmt.Errorf("%w: expected claims and headers must map nonblank names to nonblank values", ErrInvalidConfig)
	}
	v.expectedClaims = maps.Clone(cfg.expectedClaims)
	v.expectedHeaders = maps.Clone(cfg.expectedHeaders)
	return nil
}

func validMapping(m map[string]string) bool {
	for name, value := range m {
		if !authval.NonBlank(name) || !authval.NonBlank(value) {
			return false
		}
	}
	return true
}

// cognitoPoolID is the shape of a Cognito user pool ID: the Region, an
// underscore, and the pool's own identifier.
var cognitoPoolID = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d+_[A-Za-z0-9]+$`)

// NewCognitoVerifier verifies the access tokens of an Amazon Cognito user
// pool, bound to audience through Cognito's resource binding. Besides what
// NewJWTVerifier checks, the token must be an access token (token_use is
// "access") issued to clientID. Cognito ID tokens, and access tokens issued
// without the resource audience, are refused.
//
// The issuer, the RS256 algorithm and the JWKS URI follow from the pool and
// cannot be changed: WithJWKSURI is refused, WithJWKS is allowed.
func NewCognitoVerifier(userPoolID, clientID string, audience []string, opts ...Option) (*JWTVerifier, error) {
	if !cognitoPoolID.MatchString(userPoolID) {
		return nil, fmt.Errorf("%w: a valid Cognito user pool ID is required", ErrInvalidConfig)
	}
	if !authval.NonBlank(clientID) {
		return nil, fmt.Errorf("%w: a nonblank Cognito app client ID is required", ErrInvalidConfig)
	}
	cfg := newConfig(opts)
	if cfg.jwksURISet {
		return nil, fmt.Errorf("%w: the Cognito issuer, algorithm and JWKS URI cannot be overridden", ErrInvalidConfig)
	}
	region, _, _ := strings.Cut(userPoolID, "_")
	domain := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		domain = "amazonaws.com.cn"
	}
	issuer := "https://cognito-idp." + region + "." + domain + "/" + userPoolID
	if !cfg.jwksSet {
		cfg.jwksURI, cfg.jwksURISet = issuer+"/.well-known/jwks.json", true
	}
	v, err := newJWTVerifier(issuer, audience, []Algorithm{RS256}, cfg)
	if err != nil {
		return nil, err
	}
	v.cognitoClientID = clientID
	return v, nil
}

// String hides the configuration, as Powertools' repr does.
func (*JWTVerifier) String() string { return "<JWTVerifier>" }

// Issuer returns the issuer the verifier trusts.
func (v *JWTVerifier) Issuer() string { return v.issuer }

// Prefetch fetches the signing keys unless they are fresh, so that the
// first invocation does not wait for them. Call it during initialization;
// an identity provider outage then fails the cold start. It does nothing
// with a static key set. A failure matches ErrJWKSFetch.
func (v *JWTVerifier) Prefetch(ctx context.Context) error {
	if v.cache == nil {
		return nil
	}
	_, err := v.cache.get(ctx, v.timeout, "", false)
	return err
}

// Verify returns the claims of token, a JWT without the Bearer prefix.
//
// The header must name an accepted algorithm and a kid, without crit or an
// unencoded payload; exactly one published key must match it. Then the
// signature, the issuer, the audience, the required claims, exp, nbf and
// iat (with the clock skew), and the expected claims and headers are
// checked, in Powertools' order. Failures match ErrInvalidToken, or
// ErrJWKSFetch when the signing keys are unavailable.
func (v *JWTVerifier) Verify(ctx context.Context, token string) (*Claims, error) {
	header, err := v.header(token)
	if err != nil {
		return nil, err
	}
	keys := v.static
	if v.cache != nil {
		if keys, err = v.cache.get(ctx, v.timeout, header.kid, true); err != nil {
			return nil, err
		}
	}
	key, err := keys.signingKey(header.alg, header.kid)
	if err != nil {
		return nil, err
	}
	payload, err := verifySignature(token, header.alg, key)
	if err != nil {
		return nil, err
	}
	claims, err := ParseClaims(payload)
	if err != nil {
		return nil, err
	}
	if err := v.checkRegistered(claims); err != nil {
		return nil, err
	}
	if err := v.checkTimes(claims); err != nil {
		return nil, err
	}
	if err := v.checkProfile(claims, header); err != nil {
		return nil, err
	}
	return claims, nil
}

// joseHeader is the header of a token, read before its signature is
// checked: it only selects the key, and the signature covers it.
type joseHeader struct {
	members map[string]jsontext.Value
	alg     Algorithm
	kid     string
}

func (v *JWTVerifier) header(token string) (*joseHeader, error) {
	segments, ok := decodeSegments(token)
	if !ok {
		return nil, errInvalidToken()
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(segments[0], &members); err != nil || members == nil {
		return nil, errInvalidToken()
	}
	alg, _ := jsonString(members["alg"])
	kid, _ := jsonString(members["kid"])
	_, crit := members["crit"]
	if !slices.Contains(v.algorithms, Algorithm(alg)) || kid == "" || crit || members["b64"].Kind() == 'f' {
		return nil, errInvalidToken()
	}
	return &joseHeader{members: members, alg: Algorithm(alg), kid: kid}, nil
}

// segment decodes the parts of a compact JWS: canonical base64url without
// padding, as PyJWT requires.
var segment = base64.RawURLEncoding.Strict()

// decodeSegments decodes the three segments of a compact JWS.
func decodeSegments(token string) ([3][]byte, bool) {
	var out [3][]byte
	parts := strings.Split(token, ".")
	if len(parts) != len(out) {
		return out, false
	}
	for i, part := range parts {
		decoded, err := segment.DecodeString(part)
		if err != nil {
			return out, false
		}
		out[i] = decoded
	}
	return out, true
}

// verifySignature checks the signature of token with key and returns the
// payload it covers. A signature the key rejects is an invalid signature;
// anything else the JWS library refuses is a claims failure, as in PyJWT.
func verifySignature(token string, alg Algorithm, key any) ([]byte, error) {
	algorithm, ok := jwa.LookupSignatureAlgorithm(string(alg))
	if !ok {
		return nil, errInvalidToken()
	}
	payload, err := jws.Verify([]byte(token), jws.WithKey(algorithm, key))
	switch {
	case err == nil:
		return payload, nil
	case errors.Is(err, jws.VerificationError()):
		return nil, errInvalidSignature()
	default:
		return nil, errInvalidClaims()
	}
}

// checkRegistered checks the claims PyJWT checks: the required ones are
// present and not null, iss is the issuer, aud holds an accepted audience,
// and sub and jti are strings when present.
func (v *JWTVerifier) checkRegistered(c *Claims) error {
	for _, name := range v.required {
		if value, ok := c.fields[name]; !ok || value.Kind() == 'n' {
			return errInvalidClaims()
		}
	}
	if issuer, ok := jsonString(c.fields["iss"]); !ok || issuer != v.issuer {
		return errInvalidClaims()
	}
	if !v.audienceMatches(c.fields["aud"]) {
		return errInvalidClaims()
	}
	for _, name := range []string{"sub", "jti"} {
		if value, ok := c.fields[name]; ok && value.Kind() != '"' {
			return errInvalidClaims()
		}
	}
	return nil
}

// audienceMatches reports whether aud, a nonempty string or a nonempty
// list of strings, holds one of the accepted audiences.
func (v *JWTVerifier) audienceMatches(aud jsontext.Value) bool {
	var values []string
	switch aud.Kind() {
	case '"':
		s, _ := jsonString(aud)
		values = []string{s}
	case '[':
		if err := json.Unmarshal(aud, &values); err != nil {
			return false
		}
	default:
		return false
	}
	// An empty string or list counts as a missing claim.
	if len(values) == 0 || (aud.Kind() == '"' && values[0] == "") {
		return false
	}
	return slices.ContainsFunc(v.audience, func(a string) bool { return slices.Contains(values, a) })
}

// checkTimes checks exp, nbf and iat: finite numbers when present, the
// token not expired and not from the future, within the clock skew.
func (v *JWTVerifier) checkTimes(c *Claims) error {
	times := make(map[string]float64, 3)
	for _, name := range []string{"exp", "nbf", "iat"} {
		value, ok := c.fields[name]
		if !ok {
			continue
		}
		seconds, finite := jsonNumber(value)
		if !finite {
			return errInvalidClaims()
		}
		times[name] = seconds
	}
	now := float64(v.now().UnixNano()) / float64(time.Second)
	skew := v.clockSkew.Seconds()
	if times["exp"] <= now-skew {
		return errTokenExpired()
	}
	if times["nbf"] > now+skew || times["iat"] > now+skew {
		return errInvalidClaims()
	}
	return nil
}

// checkProfile checks the expected claims, then the expected headers, then
// the Cognito access token profile.
func (v *JWTVerifier) checkProfile(c *Claims, header *joseHeader) error {
	if !matchesAll(c.fields, v.expectedClaims) || !matchesAll(header.members, v.expectedHeaders) {
		return errInvalidClaims()
	}
	if v.cognitoClientID != "" {
		use, _ := c.GetString("token_use")
		client, _ := c.GetString("client_id")
		if use != "access" || client != v.cognitoClientID {
			return errInvalidClaims()
		}
	}
	return nil
}

func matchesAll(members map[string]jsontext.Value, expected map[string]string) bool {
	for name, want := range expected {
		if got, ok := jsonString(members[name]); !ok || got != want {
			return false
		}
	}
	return true
}
