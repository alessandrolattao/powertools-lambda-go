package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifierFunc is a Verifier made of a function.
type verifierFunc func(ctx context.Context, token string) (*Claims, error)

func (f verifierFunc) Verify(ctx context.Context, token string) (*Claims, error) {
	return f(ctx, token)
}
func (verifierFunc) Prefetch(context.Context) error { return nil }

// claimsVerifier accepts the token "good" with claims, and fails any other
// token with err.
func claimsVerifier(t *testing.T, claims string, err error) Verifier {
	t.Helper()
	parsed, parseErr := ParseClaims([]byte(claims))
	require.NoError(t, parseErr)
	return verifierFunc(func(_ context.Context, token string) (*Claims, error) {
		if token == "good" {
			return parsed, nil
		}
		return nil, err
	})
}

// serve runs the protected handler on a request with the given
// Authorization header, and reports whether the handler ran.
func serve(t *testing.T, protect func(http.Handler) http.Handler, authorization string) (*httptest.ResponseRecorder, *Claims) {
	t.Helper()
	var seen *Claims
	handler := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFromContext(r.Context())
		assert.True(t, ok)
		seen = claims
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com/orders", http.NoBody)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w, seen
}

func TestRequire(t *testing.T) {
	t.Parallel()
	const granted = `{"sub":"user-1","scope":"orders:read orders:write"}`
	cases := []struct {
		name          string
		verifier      Verifier
		opts          []RequireOption
		authorization string
		status        int
		challenge     string
	}{
		{"valid", claimsVerifier(t, granted, nil), []RequireOption{WithScopes("orders:read")}, "Bearer good", http.StatusNoContent, ""},
		{"missing token", claimsVerifier(t, granted, nil), nil, "", http.StatusUnauthorized, "Bearer"},
		{"wrong scheme", claimsVerifier(t, granted, nil), nil, "Basic good", http.StatusUnauthorized, `Bearer error="invalid_token"`},
		{"invalid token", claimsVerifier(t, granted, errInvalidSignature()), nil, "Bearer bad", http.StatusUnauthorized, `Bearer error="invalid_token"`},
		{"expired token", claimsVerifier(t, granted, errTokenExpired()), nil, "Bearer bad", http.StatusUnauthorized, `Bearer error="invalid_token"`},
		{
			"insufficient scope", claimsVerifier(t, granted, nil),
			[]RequireOption{WithScopes("orders:read", "orders:delete", "orders:read")},
			"Bearer good", http.StatusForbidden, `Bearer error="insufficient_scope", scope="orders:read orders:delete"`,
		},
		{"invalid scope claim", claimsVerifier(t, `{"scope":1}`, nil), nil, "Bearer good", http.StatusUnauthorized, `Bearer error="invalid_token"`},
		{
			"authorization refused", claimsVerifier(t, granted, nil),
			[]RequireOption{WithAuthorize(func(*http.Request, *Claims) bool { return false })},
			"Bearer good", http.StatusForbidden, "",
		},
		{
			"authorization granted", claimsVerifier(t, granted, nil),
			[]RequireOption{WithAuthorize(func(r *http.Request, c *Claims) bool { return c.Subject() == "user-1" && r.URL.Path == "/orders" })},
			"Bearer good", http.StatusNoContent, "",
		},
		{"keys unavailable", claimsVerifier(t, granted, errJWKSFetch()), nil, "Bearer bad", http.StatusServiceUnavailable, ""},
		{"verifier failure", claimsVerifier(t, granted, errors.New("boom")), nil, "Bearer bad", http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			protect, err := Require(tc.verifier, tc.opts...)
			require.NoError(t, err)
			w, claims := serve(t, protect, tc.authorization)
			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.challenge, w.Header().Get("WWW-Authenticate"))
			if tc.status == http.StatusNoContent {
				require.NotNil(t, claims)
				assert.Equal(t, "user-1", claims.Subject())
				return
			}
			assert.Nil(t, claims, "the handler never runs on failure")
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			assert.JSONEq(t, `{"message":"`+http.StatusText(tc.status)+`"}`, w.Body.String())
		})
	}
}

func TestRequireErrorResponse(t *testing.T) {
	t.Parallel()
	var got Failure
	protect, err := Require(claimsVerifier(t, `{}`, errJWKSFetch()), WithErrorResponse(func(w http.ResponseWriter, _ *http.Request, f Failure) {
		got = f
		w.WriteHeader(http.StatusTeapot)
	}))
	require.NoError(t, err)
	w, claims := serve(t, protect, "Bearer bad")
	assert.Nil(t, claims)
	assert.Equal(t, http.StatusTeapot, w.Code)
	assert.Equal(t, Failure{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Reason: ReasonJWKSUnavailable, Retryable: true}, got)

	protect, err = Require(claimsVerifier(t, `{}`, nil), WithErrorResponse(func(w http.ResponseWriter, _ *http.Request, f Failure) {
		got = f
		w.WriteHeader(f.StatusCode)
	}))
	require.NoError(t, err)
	w, _ = serve(t, protect, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, ReasonMissingToken, got.Reason)
	assert.Equal(t, "Bearer", got.Header.Get("WWW-Authenticate"))
}

func TestRequireRefusesInvalidScopes(t *testing.T) {
	t.Parallel()
	_, err := Require(nil)
	require.ErrorIs(t, err, ErrInvalidConfig)
	_, err = AuthorizeToken(t.Context(), nil, tokenEvent("Bearer good"))
	require.ErrorIs(t, err, ErrInvalidConfig)
	for _, scopes := range [][]string{{""}, {"a b"}, {`a"b`}, {`a\b`}, {"café"}} {
		_, err := Require(claimsVerifier(t, `{}`, nil), WithScopes(scopes...))
		require.ErrorIs(t, err, ErrInvalidConfig, "%q", scopes)
	}
}

func TestRequireWithRealTokens(t *testing.T) {
	t.Parallel()
	c := newClock()
	key := newKey(t, ES256, "k1")
	v := staticVerifier(t, c, jwksOf(t, key.jwk(t, nil)))
	protect, err := Require(v, WithScopes("orders:read"))
	require.NoError(t, err)

	w, claims := serve(t, protect, "Bearer "+key.sign(t, with(claimsAt(c.Now()), map[string]any{"scp": []string{"orders:read"}}), nil))
	assert.Equal(t, http.StatusNoContent, w.Code)
	require.NotNil(t, claims)
	assert.Equal(t, []string{"orders:read"}, claims.Scopes())

	w, _ = serve(t, protect, "Bearer "+key.sign(t, claimsAt(c.Now()), nil))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestClaimsFromContext(t *testing.T) {
	t.Parallel()
	_, ok := ClaimsFromContext(t.Context())
	assert.False(t, ok)
	_, ok = ClaimsFromContext(ContextWithClaims(t.Context(), nil))
	assert.False(t, ok)
	claims, err := ParseClaims([]byte(`{"sub":"x"}`))
	require.NoError(t, err)
	got, ok := ClaimsFromContext(ContextWithClaims(t.Context(), claims))
	assert.True(t, ok)
	assert.Same(t, claims, got)
}
