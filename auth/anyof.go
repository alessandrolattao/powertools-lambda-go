package auth

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// issuerVerifier routes a token to the verifier of its issuer.
type issuerVerifier struct {
	verifiers []*JWTVerifier
	byIssuer  map[string]*JWTVerifier
}

// AnyOf returns a Verifier for functions that trust the access tokens of
// several issuers, each with its own verifier. A token goes to the verifier
// of its iss claim, read before anything is verified: it only selects one
// of the configured verifiers, which then verifies the whole token. A token
// from any other issuer is invalid, and causes no network request.
//
// At least one verifier is required, and no two may trust the same issuer.
func AnyOf(verifiers ...*JWTVerifier) (Verifier, error) {
	if len(verifiers) == 0 {
		return nil, fmt.Errorf("%w: at least one issuer-specific verifier is required", ErrInvalidConfig)
	}
	g := &issuerVerifier{verifiers: verifiers, byIssuer: make(map[string]*JWTVerifier, len(verifiers))}
	for _, v := range verifiers {
		if v == nil {
			return nil, fmt.Errorf("%w: at least one issuer-specific verifier is required", ErrInvalidConfig)
		}
		if _, ok := g.byIssuer[v.issuer]; ok {
			return nil, fmt.Errorf("%w: two verifiers for one issuer are ambiguous", ErrInvalidConfig)
		}
		g.byIssuer[v.issuer] = v
	}
	return g, nil
}

// String hides the configuration, as Powertools' repr does.
func (*issuerVerifier) String() string { return "<JWTVerifier issuer group>" }

// Verify verifies token with the verifier of its issuer.
func (g *issuerVerifier) Verify(ctx context.Context, token string) (*Claims, error) {
	segments, ok := decodeSegments(token)
	if !ok {
		return nil, errInvalidToken()
	}
	var header, payload map[string]jsontext.Value
	if json.Unmarshal(segments[0], &header) != nil || header == nil ||
		json.Unmarshal(segments[1], &payload) != nil || payload == nil {
		return nil, errInvalidToken()
	}
	issuer, _ := jsonString(payload["iss"])
	v, ok := g.byIssuer[issuer]
	if !ok {
		return nil, errInvalidToken()
	}
	return v.Verify(ctx, token)
}

// Prefetch prefetches the keys of every verifier, stopping at the first
// failure.
func (g *issuerVerifier) Prefetch(ctx context.Context) error {
	for _, v := range g.verifiers {
		if err := v.Prefetch(ctx); err != nil {
			return err
		}
	}
	return nil
}
