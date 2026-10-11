package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyEveryAlgorithm(t *testing.T) {
	t.Parallel()
	for _, alg := range asymmetric {
		t.Run(string(alg), func(t *testing.T) {
			t.Parallel()
			c := newClock()
			key := newKey(t, alg, "k-"+string(alg))
			v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)))

			claims, err := v.Verify(t.Context(), key.sign(t, claimsAt(c.Now()), nil))
			require.NoError(t, err)
			assert.Equal(t, "user-1", claims.Subject())
			assert.Equal(t, testIssuer, claims.Issuer())
			assert.Equal(t, []string{testAudience}, claims.Audience())
			assert.Equal(t, c.Now().Add(time.Hour), claims.ExpiresAt())
		})
	}
}

func TestVerifyRefusesHeaders(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, RS256, "k1")
	v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)), WithClockSkew(0))
	valid := key.sign(t, claimsAt(c.Now()), nil)
	parts := strings.Split(valid, ".")

	cases := map[string]string{
		"empty":              "",
		"two segments":       parts[0] + "." + parts[1],
		"four segments":      valid + ".x",
		"padded header":      parts[0] + "=." + parts[1] + "." + parts[2],
		"bad base64 payload": parts[0] + ".!!." + parts[2],
		"bad signature b64":  parts[0] + "." + parts[1] + ".*",
		"header not object":  key.signRaw(t, []byte(`["RS256"]`), []byte(`{}`)),
		"header not JSON":    key.signRaw(t, []byte(`{"alg":`), []byte(`{}`)),
		"duplicate alg":      key.signRaw(t, []byte(`{"alg":"RS256","alg":"RS256","kid":"k1"}`), []byte(`{}`)),
		"alg none":           key.sign(t, claimsAt(c.Now()), map[string]any{"alg": "none"}),
		"alg HS256":          key.sign(t, claimsAt(c.Now()), map[string]any{"alg": "HS256"}),
		"alg missing":        key.sign(t, claimsAt(c.Now()), map[string]any{"alg": nil}),
		"kid missing":        key.sign(t, claimsAt(c.Now()), map[string]any{"kid": nil}),
		"kid empty":          key.sign(t, claimsAt(c.Now()), map[string]any{"kid": ""}),
		"kid number":         key.sign(t, claimsAt(c.Now()), map[string]any{"kid": 1}),
		"crit list":          key.sign(t, claimsAt(c.Now()), map[string]any{"crit": []string{"exp"}, "exp": 1}),
		"crit empty":         key.sign(t, claimsAt(c.Now()), map[string]any{"crit": []string{}}),
		"crit null":          key.signRaw(t, []byte(`{"alg":"RS256","kid":"k1","crit":null}`), []byte(`{}`)),
		"unencoded payload":  key.sign(t, claimsAt(c.Now()), map[string]any{"b64": false, "crit": nil}),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := v.Verify(t.Context(), token)
			requireReason(t, err, ReasonInvalidToken)
			require.ErrorIs(t, err, ErrInvalidToken)
		})
	}

	t.Run("b64 true is the default encoding", func(t *testing.T) {
		t.Parallel()
		_, err := v.Verify(t.Context(), key.sign(t, claimsAt(c.Now()), map[string]any{"b64": true}))
		require.NoError(t, err)
	})
}

func TestVerifySelectsExactlyOneKey(t *testing.T) {
	t.Parallel()
	c := newClock()
	rsaKey := newKey(t, RS256, "k1")
	ecKey := newKey(t, ES256, "k1")
	p384 := newKey(t, ES384, "k1")
	token := rsaKey.sign(t, claimsAt(c.Now()), nil)

	cases := []struct {
		name string
		keys []map[string]any
		ok   bool
	}{
		{"one match", []map[string]any{rsaKey.jwk(t, nil)}, true},
		{"alg matches", []map[string]any{rsaKey.jwk(t, map[string]any{"alg": "RS256"})}, true},
		{"alg null", []map[string]any{rsaKey.jwk(t, map[string]any{"alg": jsonNull{}})}, true},
		{"use sig", []map[string]any{rsaKey.jwk(t, map[string]any{"use": "sig"})}, true},
		{"key_ops verify", []map[string]any{rsaKey.jwk(t, map[string]any{"key_ops": []string{"sign", "verify"}})}, true},
		{"other kids ignored", []map[string]any{rsaKey.jwk(t, nil), rotatedRSA("k2").jwk(t, nil)}, true},
		{"no kid match", []map[string]any{rsaKey.jwk(t, map[string]any{"kid": "k2"})}, false},
		{"kid number", []map[string]any{rsaKey.jwk(t, map[string]any{"kid": 1})}, false},
		{"two matches", []map[string]any{rsaKey.jwk(t, nil), rotatedRSA("k1").jwk(t, nil)}, false},
		{"other alg", []map[string]any{rsaKey.jwk(t, map[string]any{"alg": "PS256"})}, false},
		{"use enc", []map[string]any{rsaKey.jwk(t, map[string]any{"use": "enc"})}, false},
		{"key_ops without verify", []map[string]any{rsaKey.jwk(t, map[string]any{"key_ops": []string{"encrypt"}})}, false},
		{"key_ops null", []map[string]any{rsaKey.jwk(t, map[string]any{"key_ops": jsonNull{}})}, false},
		{"key_ops string", []map[string]any{rsaKey.jwk(t, map[string]any{"key_ops": "verify"})}, false},
		{"symmetric key", []map[string]any{{"kty": "oct", "kid": "k1", "k": "c2VjcmV0"}}, false},
		{"key of another type", []map[string]any{ecKey.jwk(t, nil)}, false},
		{"malformed modulus", []map[string]any{rsaKey.jwk(t, map[string]any{"n": "!"})}, false},
		{"missing modulus", []map[string]any{rsaKey.jwk(t, map[string]any{"n": nil})}, false},
		{"usable and unusable match", []map[string]any{rsaKey.jwk(t, nil), {"kty": "RSA", "kid": "k1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := staticVerifier(t, c, jwksOf(t, tc.keys...))
			_, err := v.Verify(t.Context(), token)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			requireReason(t, err, ReasonInvalidToken)
		})
	}

	t.Run("curve must fit the algorithm", func(t *testing.T) {
		t.Parallel()
		// An ES256 token with the kid of a P-384 key.
		v := staticVerifier(t, c, jwksOf(t, p384.jwk(t, nil)))
		forged := &testKey{alg: ES256, kid: "k1", signer: p384.signer}
		_, err := v.Verify(t.Context(), forged.sign(t, claimsAt(c.Now()), nil))
		requireReason(t, err, ReasonInvalidToken)
	})

	t.Run("a private JWK verifies with its public half", func(t *testing.T) {
		t.Parallel()
		private := newKey(t, ES256, "k1")
		v := staticVerifier(t, c, jwksOf(t, privateJWK(t, private)))
		_, err := v.Verify(t.Context(), private.sign(t, claimsAt(c.Now()), nil))
		require.NoError(t, err)
	})
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)))
	token := key.sign(t, claimsAt(c.Now()), nil)

	t.Run("tampered payload", func(t *testing.T) {
		t.Parallel()
		parts := strings.Split(token, ".")
		forged := parts[0] + "." + b64([]byte(`{"sub":"admin"}`)) + "." + parts[2]
		_, err := v.Verify(t.Context(), forged)
		requireReason(t, err, ReasonInvalidSignature)
		require.ErrorIs(t, err, ErrInvalidToken)
	})
	t.Run("another key under the same kid", func(t *testing.T) {
		t.Parallel()
		impostor := newKey(t, ES256, "k1")
		_, err := v.Verify(t.Context(), impostor.sign(t, claimsAt(c.Now()), nil))
		requireReason(t, err, ReasonInvalidSignature)
	})
	t.Run("truncated signature", func(t *testing.T) {
		t.Parallel()
		_, err := v.Verify(t.Context(), token[:strings.LastIndex(token, ".")+5])
		requireReason(t, err, ReasonInvalidSignature)
	})
}

func TestVerifyClaims(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, EdDSA, "k1")
	valid := claimsAt(c.Now())

	cases := []struct {
		name   string
		claims any
		opts   []Option
		reason Reason
	}{
		{"valid", valid, nil, ""},
		{"payload not an object", []byte(`["x"]`), nil, ReasonInvalidClaims},
		{"payload not JSON", []byte(`{"iss":`), nil, ReasonInvalidClaims},
		{"payload empty", []byte(``), nil, ReasonInvalidClaims},
		{"duplicate claim", []byte(`{"iss":"a","iss":"b"}`), nil, ReasonInvalidClaims},
		{"iss missing", with(valid, map[string]any{"iss": nil}), nil, ReasonInvalidClaims},
		{"iss other", with(valid, map[string]any{"iss": "https://evil.example.com/"}), nil, ReasonInvalidClaims},
		{"iss without slash", with(valid, map[string]any{"iss": "https://idp.example.com"}), nil, ReasonInvalidClaims},
		{"iss number", with(valid, map[string]any{"iss": 1}), nil, ReasonInvalidClaims},
		{"aud missing", with(valid, map[string]any{"aud": nil}), nil, ReasonInvalidClaims},
		{"aud null", with(valid, map[string]any{"aud": jsonNull{}}), nil, ReasonInvalidClaims},
		{"aud other", with(valid, map[string]any{"aud": "https://other.example.com"}), nil, ReasonInvalidClaims},
		{"aud empty string", with(valid, map[string]any{"aud": ""}), nil, ReasonInvalidClaims},
		{"aud empty list", with(valid, map[string]any{"aud": []string{}}), nil, ReasonInvalidClaims},
		{"aud list", with(valid, map[string]any{"aud": []string{"x", testAudience}}), nil, ""},
		{"aud list without match", with(valid, map[string]any{"aud": []string{"x", "y"}}), nil, ReasonInvalidClaims},
		{"aud list with a number", with(valid, map[string]any{"aud": []any{testAudience, 1}}), nil, ReasonInvalidClaims},
		{"aud object", with(valid, map[string]any{"aud": map[string]any{"a": 1}}), nil, ReasonInvalidClaims},
		{"exp missing", with(valid, map[string]any{"exp": nil}), nil, ReasonInvalidClaims},
		{"exp null", with(valid, map[string]any{"exp": jsonNull{}}), nil, ReasonInvalidClaims},
		{"required sub missing", with(valid, map[string]any{"sub": nil}), []Option{WithRequiredClaims("sub")}, ReasonInvalidClaims},
		{"required sub null", with(valid, map[string]any{"sub": jsonNull{}}), []Option{WithRequiredClaims("sub")}, ReasonInvalidClaims},
		{"required tenant present", with(valid, map[string]any{"tenant": "t"}), []Option{WithRequiredClaims("tenant")}, ""},
		{"sub number", with(valid, map[string]any{"sub": 1}), nil, ReasonInvalidClaims},
		{"jti number", with(valid, map[string]any{"jti": 1}), nil, ReasonInvalidClaims},
		{"jti string", with(valid, map[string]any{"jti": "j"}), nil, ""},
		{"second audience accepted", with(valid, map[string]any{"aud": "https://second.example.com"}), nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := NewJWTVerifier(testIssuer, []string{testAudience, "https://second.example.com"}, []Algorithm{EdDSA},
				append([]Option{WithJWKS(jwksOf(t, key.jwk(t, nil))), withClock(c.Now)}, tc.opts...)...)
			require.NoError(t, err)
			_, err = v.Verify(t.Context(), key.sign(t, tc.claims, nil))
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			requireReason(t, err, tc.reason)
		})
	}
}

func TestVerifyTimes(t *testing.T) {
	t.Parallel()
	c := newClock()
	now := c.Now().Unix()
	key := newKey(t, ES384, "k1")
	valid := claimsAt(c.Now())

	cases := []struct {
		name   string
		claims map[string]any
		skew   time.Duration
		reason Reason
	}{
		{"expired beyond the skew", with(valid, map[string]any{"exp": now - 61}), DefaultClockSkew, ReasonTokenExpired},
		{"expired at the skew boundary", with(valid, map[string]any{"exp": now - 60}), DefaultClockSkew, ReasonTokenExpired},
		{"expired within the skew", with(valid, map[string]any{"exp": now - 59}), DefaultClockSkew, ""},
		{"expiring now without skew", with(valid, map[string]any{"exp": now}), 0, ReasonTokenExpired},
		{"fractional exp", with(valid, map[string]any{"exp": float64(now) + 0.5}), 0, ""},
		{"nbf beyond the skew", with(valid, map[string]any{"nbf": now + 61}), DefaultClockSkew, ReasonInvalidClaims},
		{"nbf within the skew", with(valid, map[string]any{"nbf": now + 60}), DefaultClockSkew, ""},
		{"iat in the future", with(valid, map[string]any{"iat": now + 61}), DefaultClockSkew, ReasonInvalidClaims},
		{"iat within the skew", with(valid, map[string]any{"iat": now + 30}), DefaultClockSkew, ""},
		{"exp string", with(valid, map[string]any{"exp": "2100000000"}), DefaultClockSkew, ReasonInvalidClaims},
		{"exp boolean", with(valid, map[string]any{"exp": true}), DefaultClockSkew, ReasonInvalidClaims},
		{"exp overflowing", with(valid, map[string]any{"exp": jsonRaw("1e400")}), DefaultClockSkew, ReasonInvalidClaims},
		{"nbf null", with(valid, map[string]any{"nbf": jsonNull{}}), DefaultClockSkew, ReasonInvalidClaims},
		{"iat list", with(valid, map[string]any{"iat": []int{1}}), DefaultClockSkew, ReasonInvalidClaims},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)), WithClockSkew(tc.skew))
			_, err := v.Verify(t.Context(), key.sign(t, tc.claims, nil))
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			requireReason(t, err, tc.reason)
			require.ErrorIs(t, err, ErrInvalidToken)
		})
	}

	t.Run("an audience mismatch wins over expiry, as in PyJWT", func(t *testing.T) {
		t.Parallel()
		v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)))
		_, err := v.Verify(t.Context(), key.sign(t, with(valid, map[string]any{"exp": now - 3600, "aud": "x"}), nil))
		requireReason(t, err, ReasonInvalidClaims)
	})
}

func TestVerifyProfile(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, PS256, "k1")
	jwks := jwksOf(t, key.jwk(t, nil))
	access := with(claimsAt(c.Now()), map[string]any{"token_use": "access"})
	v := staticVerifier(t, c, jwks,
		WithExpectedClaims(map[string]string{"token_use": "access"}),
		WithExpectedHeaders(map[string]string{"typ": "at+jwt"}))

	cases := []struct {
		name   string
		claims map[string]any
		header map[string]any
		ok     bool
	}{
		{"matching", access, map[string]any{"typ": "at+jwt"}, true},
		{"claim mismatch", with(access, map[string]any{"token_use": "id"}), map[string]any{"typ": "at+jwt"}, false},
		{"claim case differs", with(access, map[string]any{"token_use": "Access"}), map[string]any{"typ": "at+jwt"}, false},
		{"claim missing", with(access, map[string]any{"token_use": nil}), map[string]any{"typ": "at+jwt"}, false},
		{"claim not a string", with(access, map[string]any{"token_use": 1}), map[string]any{"typ": "at+jwt"}, false},
		{"header mismatch", access, map[string]any{"typ": "JWT"}, false},
		{"header missing", access, map[string]any{"typ": nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := v.Verify(t.Context(), key.sign(t, tc.claims, tc.header))
			if tc.ok {
				require.NoError(t, err)
				return
			}
			requireReason(t, err, ReasonInvalidClaims)
		})
	}
}

func TestCognitoVerifier(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := rotatedRSA("cognito-key")
	jwks := jwksOf(t, key.jwk(t, nil))
	const pool = "eu-west-1_AbC123"
	issuer := "https://cognito-idp.eu-west-1.amazonaws.com/" + pool
	v, err := NewCognitoVerifier(pool, "orders-client", []string{testAudience}, WithJWKS(jwks), withClock(c.Now))
	require.NoError(t, err)
	assert.Equal(t, issuer, v.Issuer())
	access := with(claimsAt(c.Now()), map[string]any{"iss": issuer, "token_use": "access", "client_id": "orders-client"})

	cases := []struct {
		name   string
		claims map[string]any
		ok     bool
	}{
		{"resource-bound access token", access, true},
		{"ID token", with(access, map[string]any{"token_use": "id"}), false},
		{"another app client", with(access, map[string]any{"client_id": "other"}), false},
		{"no client_id", with(access, map[string]any{"client_id": nil}), false},
		{"unbound access token", with(access, map[string]any{"aud": nil}), false},
		{"other pool", with(access, map[string]any{"iss": issuer + "x"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := v.Verify(t.Context(), key.sign(t, tc.claims, nil))
			if tc.ok {
				require.NoError(t, err)
				return
			}
			requireReason(t, err, ReasonInvalidClaims)
		})
	}

	t.Run("only RS256", func(t *testing.T) {
		t.Parallel()
		ec := newKey(t, ES256, "cognito-key")
		other, err := NewCognitoVerifier(pool, "orders-client", []string{testAudience}, WithJWKS(jwksOf(t, ec.jwk(t, nil))), withClock(c.Now))
		require.NoError(t, err)
		_, err = other.Verify(t.Context(), ec.sign(t, access, nil))
		requireReason(t, err, ReasonInvalidToken)
	})

	t.Run("China regions", func(t *testing.T) {
		t.Parallel()
		cn, err := NewCognitoVerifier("cn-north-1_Pool9", "client", []string{testAudience})
		require.NoError(t, err)
		assert.Equal(t, "https://cognito-idp.cn-north-1.amazonaws.com.cn/cn-north-1_Pool9", cn.Issuer())
		assert.Equal(t, "https://cognito-idp.cn-north-1.amazonaws.com.cn/cn-north-1_Pool9/.well-known/jwks.json", cn.cache.id.uri)
	})

	for name, build := range map[string]func() (*JWTVerifier, error){
		"pool without region": func() (*JWTVerifier, error) { return NewCognitoVerifier("AbC123", "c", []string{testAudience}) },
		"pool upper case":     func() (*JWTVerifier, error) { return NewCognitoVerifier("EU-WEST-1_x", "c", []string{testAudience}) },
		"blank client":        func() (*JWTVerifier, error) { return NewCognitoVerifier(pool, " ", []string{testAudience}) },
		"JWKS URI override": func() (*JWTVerifier, error) {
			return NewCognitoVerifier(pool, "c", []string{testAudience}, WithJWKSURI("https://evil.example.com/keys"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := build()
			require.ErrorIs(t, err, ErrInvalidConfig)
		})
	}
}

func TestNewJWTVerifierRefusesConfiguration(t *testing.T) {
	t.Parallel()
	good := []byte(`{"keys":[]}`)
	cases := []struct {
		name       string
		issuer     string
		audience   []string
		algorithms []Algorithm
		opts       []Option
	}{
		{"http issuer", "http://idp.example.com/", nil, nil, nil},
		{"issuer with query", "https://idp.example.com/?tenant=1", nil, nil, nil},
		{"issuer with fragment", "https://idp.example.com/#x", nil, nil, nil},
		{"issuer with user", "https://user@idp.example.com/", nil, nil, nil},
		{"issuer with password", "https://user:secret@idp.example.com/", nil, nil, nil},
		{"issuer with space", "https://idp.example.com/a b", nil, nil, nil},
		{"issuer with tab", "https://idp.example.com/\t", nil, nil, nil},
		{"issuer with no-break space", "https://idp.example.com/ ", nil, nil, nil},
		{"issuer without host", "https:///path", nil, nil, nil},
		{"issuer with bad port", "https://idp.example.com:99999/", nil, nil, nil},
		{"issuer relative", "/idp", nil, nil, nil},
		{"no audience", testIssuer, []string{}, nil, nil},
		{"blank audience", testIssuer, []string{" "}, nil, nil},
		{"no algorithm", testIssuer, nil, []Algorithm{}, nil},
		{"HS256", testIssuer, nil, []Algorithm{RS256, "HS256"}, nil},
		{"none", testIssuer, nil, []Algorithm{"none"}, nil},
		{"jwks and jwks URI", testIssuer, nil, nil, []Option{WithJWKS(good), WithJWKSURI("https://idp.example.com/keys")}},
		{"jwks not JSON", testIssuer, nil, nil, []Option{WithJWKS([]byte(`{`))}},
		{"jwks without keys", testIssuer, nil, nil, []Option{WithJWKS([]byte(`{}`))}},
		{"jwks keys not a list", testIssuer, nil, nil, []Option{WithJWKS([]byte(`{"keys":{}}`))}},
		{"jwks key not an object", testIssuer, nil, nil, []Option{WithJWKS([]byte(`{"keys":[1]}`))}},
		{"jwks URI over http", testIssuer, nil, nil, []Option{WithJWKSURI("http://idp.example.com/keys")}},
		{"negative clock skew", testIssuer, nil, nil, []Option{WithClockSkew(-time.Second)}},
		{"zero timeout", testIssuer, nil, nil, []Option{WithTimeout(0)}},
		{"zero max age", testIssuer, nil, nil, []Option{WithJWKSMaxAge(0)}},
		{"negative cooldown", testIssuer, nil, nil, []Option{WithUnknownKIDCooldown(-time.Second)}},
		{"blank required claim", testIssuer, nil, nil, []Option{WithRequiredClaims("")}},
		{"blank expected claim", testIssuer, nil, nil, []Option{WithExpectedClaims(map[string]string{"token_use": ""})}},
		{"blank expected header", testIssuer, nil, nil, []Option{WithExpectedHeaders(map[string]string{"": "x"})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			audience := tc.audience
			if audience == nil {
				audience = []string{testAudience}
			}
			algorithms := tc.algorithms
			if algorithms == nil {
				algorithms = []Algorithm{RS256}
			}
			_, err := NewJWTVerifier(tc.issuer, audience, algorithms, tc.opts...)
			require.ErrorIs(t, err, ErrInvalidConfig)
			assert.NotContains(t, err.Error(), "example.com", "errors never quote the configuration")
		})
	}

	t.Run("accepted variations", func(t *testing.T) {
		t.Parallel()
		for _, issuer := range []string{"HTTPS://idp.example.com/", "https://idp.example.com:8443/tenant", "https://[::1]/", "https://idp.example.com/?"} {
			_, err := NewJWTVerifier(issuer, []string{"a", "a"}, []Algorithm{RS256, RS256}, WithJWKS(good), WithUnknownKIDCooldown(0), WithClockSkew(0))
			require.NoError(t, err, issuer)
		}
	})
}

func TestVerifierHidesItsConfiguration(t *testing.T) {
	t.Parallel()
	v := staticVerifier(t, newClock(), []byte(`{"keys":[]}`))
	assert.Equal(t, "<JWTVerifier>", fmt.Sprintf("%+v", v), "logging a verifier shows no configuration")
	g, err := AnyOf(v)
	require.NoError(t, err)
	assert.Equal(t, "<JWTVerifier issuer group>", fmt.Sprint(g))
	require.NoError(t, v.Prefetch(t.Context()), "static keys need no prefetch")
}

func TestAnyOf(t *testing.T) {
	t.Parallel()
	c := newClock()
	first := newKey(t, ES256, "a")
	second := newKey(t, EdDSA, "b")
	v1 := staticVerifier(t, c, jwksOf(t, first.jwk(t, nil)))
	v2, err := NewJWTVerifier("https://other.example.com/", []string{testAudience}, []Algorithm{EdDSA},
		WithJWKS(jwksOf(t, second.jwk(t, nil))), withClock(c.Now))
	require.NoError(t, err)
	g, err := AnyOf(v1, v2)
	require.NoError(t, err)

	claims, err := g.Verify(t.Context(), first.sign(t, claimsAt(c.Now()), nil))
	require.NoError(t, err)
	assert.Equal(t, testIssuer, claims.Issuer())

	claims, err = g.Verify(t.Context(), second.sign(t, with(claimsAt(c.Now()), map[string]any{"iss": "https://other.example.com/"}), nil))
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.com/", claims.Issuer())

	for name, token := range map[string]string{
		"unknown issuer":        first.sign(t, with(claimsAt(c.Now()), map[string]any{"iss": "https://unknown.example.com/"}), nil),
		"issuer not a string":   first.sign(t, with(claimsAt(c.Now()), map[string]any{"iss": 1}), nil),
		"no issuer":             first.sign(t, with(claimsAt(c.Now()), map[string]any{"iss": nil}), nil),
		"payload not an object": first.sign(t, []byte(`[]`), nil),
		"not a JWT":             "abc",
		"empty":                 "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := g.Verify(t.Context(), token)
			requireReason(t, err, ReasonInvalidToken)
		})
	}

	t.Run("the routed verifier checks everything", func(t *testing.T) {
		t.Parallel()
		// Signed by the first issuer's key, claiming the second issuer.
		_, err := g.Verify(t.Context(), first.sign(t, with(claimsAt(c.Now()), map[string]any{"iss": "https://other.example.com/"}), nil))
		requireReason(t, err, ReasonInvalidToken)
	})

	t.Run("configuration", func(t *testing.T) {
		t.Parallel()
		_, err := AnyOf()
		require.ErrorIs(t, err, ErrInvalidConfig)
		_, err = AnyOf(v1, nil)
		require.ErrorIs(t, err, ErrInvalidConfig)
		_, err = AnyOf(v1, staticVerifier(t, c, jwksOf(t, first.jwk(t, nil))))
		require.ErrorIs(t, err, ErrInvalidConfig)
	})
}

func TestErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason    Reason
		is        []error
		isNot     []error
		message   string
		retryable bool
	}{
		{ReasonMissingToken, []error{ErrMissingToken, ErrInvalidToken}, []error{ErrForbidden, ErrJWKSFetch}, "missing access token", false},
		{ReasonInvalidToken, []error{ErrInvalidToken}, []error{ErrMissingToken, ErrInvalidClaims}, "invalid access token", false},
		{ReasonInvalidClaims, []error{ErrInvalidClaims, ErrInvalidToken}, []error{ErrTokenExpired}, "invalid access token claims", false},
		{ReasonTokenExpired, []error{ErrTokenExpired, ErrInvalidToken}, []error{ErrInvalidClaims}, "access token expired", false},
		{ReasonInvalidSignature, []error{ErrInvalidSignature, ErrInvalidToken}, []error{ErrForbidden}, "invalid access token signature", false},
		{ReasonForbidden, []error{ErrForbidden}, []error{ErrInsufficientScope, ErrInvalidToken}, "forbidden", false},
		{ReasonInsufficientScope, []error{ErrInsufficientScope, ErrForbidden}, []error{ErrInvalidToken}, "insufficient scope", false},
		{ReasonJWKSUnavailable, []error{ErrJWKSFetch}, []error{ErrInvalidToken, ErrForbidden}, "unable to retrieve verification keys", true},
	}
	built := map[Reason]error{
		ReasonMissingToken: errMissingToken(), ReasonInvalidToken: errInvalidToken(), ReasonInvalidClaims: errInvalidClaims(),
		ReasonTokenExpired: errTokenExpired(), ReasonInvalidSignature: errInvalidSignature(), ReasonForbidden: errForbidden(),
		ReasonInsufficientScope: errInsufficientScope(), ReasonJWKSUnavailable: errJWKSFetch(),
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			err := fmt.Errorf("wrapped: %w", built[tc.reason])
			for _, target := range tc.is {
				require.ErrorIs(t, err, target)
			}
			for _, target := range tc.isNot {
				require.NotErrorIs(t, err, target)
			}
			authErr, ok := errors.AsType[*Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.message, authErr.Error())
			assert.Equal(t, tc.retryable, authErr.Retryable)
			assert.Equal(t, tc.reason, authErr.Reason)
		})
	}
}

func TestVerifyHonoursTheCallerContext(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Static keys need no network: a done context does not matter.
	_, err := v.Verify(ctx, key.sign(t, claimsAt(c.Now()), nil))
	require.NoError(t, err)
}
