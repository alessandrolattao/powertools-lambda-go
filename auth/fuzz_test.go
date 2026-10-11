package auth

import (
	"errors"
	"strings"
	"testing"
)

// FuzzVerify feeds arbitrary tokens to a verifier and to an issuer group:
// whatever a client sends, they return claims or an *Error, never panic.
func FuzzVerify(f *testing.F) {
	c := newClock()
	key := newKey(f, ES256, "k1")
	ed := newKey(f, EdDSA, "k2")
	jwks := jwksOf(f, key.jwk(f, nil), ed.jwk(f, nil), map[string]any{"kty": "RSA", "kid": "k3", "n": "AQAB", "e": "AQAB"})
	v := staticVerifier(f, c, jwks, WithExpectedHeaders(map[string]string{"typ": "JWT"}))
	group, err := AnyOf(v)
	if err != nil {
		f.Fatal(err)
	}

	valid := key.sign(f, claimsAt(c.Now()), nil)
	f.Add(valid)
	f.Add(ed.sign(f, claimsAt(c.Now()), nil))
	f.Add(key.sign(f, []byte(`{"iss":"`+testIssuer+`","aud":[],"exp":1e400}`), nil))
	f.Add(key.signRaw(f, []byte(`{"alg":"ES256","kid":"k1","crit":["b64"],"b64":false}`), []byte(`{}`)))
	f.Add(key.signRaw(f, []byte(`{"alg":"RS256","kid":"k3"}`), []byte(`{}`)))
	f.Add(strings.Replace(valid, ".", "..", 1))
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add("")
	f.Fuzz(func(t *testing.T, token string) {
		for _, verifier := range []Verifier{v, group} {
			claims, err := verifier.Verify(t.Context(), token)
			if err == nil {
				if claims == nil {
					t.Fatal("no claims and no error")
				}
				continue
			}
			if authErr, ok := errors.AsType[*Error](err); !ok || authErr == nil {
				t.Fatalf("not an *auth.Error: %v", err)
			}
		}
	})
}

// FuzzKeySet feeds arbitrary JWKS documents to the key selection: an
// identity provider's answer never makes it panic.
func FuzzKeySet(f *testing.F) {
	f.Add([]byte(`{"keys":[{"kty":"EC","kid":"k","crv":"P-256","x":"AA","y":"AA"}]}`), "k")
	f.Add([]byte(`{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB","alg":null,"key_ops":["verify"]}]}`), "k")
	f.Add([]byte(`{"keys":[{"kty":"OKP","kid":"k","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}]}`), "k")
	f.Add([]byte(`{"keys":[{"kty":"OKP","kid":"k","crv":"X25519","x":"AA"},{"kty":"oct","kid":"k"}]}`), "k")
	f.Fuzz(func(_ *testing.T, raw []byte, kid string) {
		set, err := parseKeySetJSON(raw)
		if err != nil {
			return
		}
		set.hasKID(kid)
		for _, alg := range asymmetric {
			_, _ = set.signingKey(alg, kid) //nolint:errcheck // the property is the absence of panics
		}
	})
}

// FuzzEventToken feeds arbitrary header values to the bearer extraction.
func FuzzEventToken(f *testing.F) {
	f.Add("Authorization", "Bearer abc", "authorization", "Bearer abc")
	f.Add("AUTHORIZATION", "bearer\ta", "x", "")
	f.Fuzz(func(t *testing.T, name, value, multiName, multiValue string) {
		token, err := eventToken(map[string]string{name: value}, map[string][]string{multiName: {multiValue}})
		if err != nil {
			if authErr, ok := errors.AsType[*Error](err); !ok || authErr == nil {
				t.Fatalf("not an *auth.Error: %v", err)
			}
			return
		}
		if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r == ' ' || r == '\t' }) {
			t.Fatalf("a token with whitespace: %q", token)
		}
	})
}
