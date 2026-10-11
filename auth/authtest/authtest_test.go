package authtest_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/auth"
	"github.com/alessandrolattao/powertools-lambda-go/auth/authtest"
)

func TestVerifier(t *testing.T) {
	t.Parallel()
	v := authtest.NewVerifier(map[string]any{"sub": "user-1", "scope": "orders:read"})
	require.NoError(t, v.Prefetch(t.Context()))

	first, err := v.Verify(t.Context(), "anything")
	require.NoError(t, err)
	second, err := v.Verify(t.Context(), "else")
	require.NoError(t, err)
	assert.Equal(t, "user-1", first.Subject())
	assert.NotSame(t, first, second, "every call gets its own copy")

	type claims struct {
		Subject string `json:"sub"`
	}
	fromStruct, err := authtest.NewVerifier(claims{Subject: "user-2"}).Verify(t.Context(), "x")
	require.NoError(t, err)
	assert.Equal(t, "user-2", fromStruct.Subject())
}

func TestVerifierStillChecksTheRequest(t *testing.T) {
	t.Parallel()
	protect, err := auth.Require(authtest.NewVerifier(map[string]any{"sub": "user-1", "scope": "orders:read"}), auth.WithScopes("orders:write"))
	require.NoError(t, err)
	handler := protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	for authorization, status := range map[string]int{"": http.StatusUnauthorized, "Bearer x": http.StatusForbidden} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/orders", http.NoBody)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, status, w.Code, authorization)
	}
}

func TestNewVerifierPanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { authtest.NewVerifier([]string{"not", "an", "object"}) })
	assert.Panics(t, func() { authtest.NewVerifier(func() {}) })
}

func ExampleNewVerifier() {
	verifier := authtest.NewVerifier(map[string]any{"sub": "test-user", "scope": "orders:read"})
	protect, err := auth.Require(verifier, auth.WithScopes("orders:read"))
	if err != nil {
		panic(err)
	}
	handler := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.ClaimsFromContext(r.Context())
		w.Header().Set("X-Subject", claims.Subject())
	}))

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/orders", http.NoBody)
	r.Header.Set("Authorization", "Bearer any-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	fmt.Println(w.Code, w.Header().Get("X-Subject"))
	// Output: 200 test-user
}
