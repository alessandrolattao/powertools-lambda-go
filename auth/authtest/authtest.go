// Package authtest tests code protected by the auth package without real
// tokens, keys or network: the Go side of Powertools Python's mock_claims.
//
// A Verifier answers every token with the same claims. Everything around
// the verification still runs: Require still extracts the bearer token and
// checks the scopes, the Authorize functions still build their policies.
//
//	verifier := authtest.NewVerifier(map[string]any{"sub": "user-1", "scope": "orders:read"})
//	protect, _ := auth.Require(verifier, auth.WithScopes("orders:read"))
//
// It bypasses every security check, so use it in tests only, and keep
// separate tests for the token profiles the application accepts.
package authtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/auth"
)

// Verifier is an auth.Verifier that trusts every token.
type Verifier struct {
	claims jsontext.Value
}

var _ auth.Verifier = (*Verifier)(nil)

// NewVerifier returns a Verifier whose Verify returns claims, a map or a
// struct that marshals to a JSON object. It panics otherwise, as befits a
// test helper given a constant.
func NewVerifier(claims any) *Verifier {
	raw, err := json.Marshal(claims, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("authtest: the claims do not marshal to JSON: %v", err))
	}
	if _, err := auth.ParseClaims(raw); err != nil {
		panic("authtest: the claims must marshal to a JSON object")
	}
	return &Verifier{claims: raw}
}

// Verify returns a fresh copy of the claims, whatever the token.
func (v *Verifier) Verify(context.Context, string) (*auth.Claims, error) {
	return auth.ParseClaims(v.claims)
}

// Prefetch does nothing: there are no keys to fetch.
func (*Verifier) Prefetch(context.Context) error { return nil }
