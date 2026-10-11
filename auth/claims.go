package auth

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
)

// Claims are the claims of an access token, as Verify returns them once the
// signature, the issuer, the audience and the times have been checked.
// Claims are immutable and safe for concurrent use.
//
// The accessors read the registered claims; Get, GetString and Lookup read
// any other one, and As decodes them all into a struct of yours.
type Claims struct {
	raw    jsontext.Value
	fields map[string]jsontext.Value
}

// ParseClaims decodes a JWT claims set, which must be a JSON object. It
// verifies nothing: it is for Verifier implementations and for tests, while
// Verify is how claims come to be trusted.
func ParseClaims(payload []byte) (*Claims, error) {
	raw := jsontext.Value(slices.Clone(payload))
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errInvalidClaims()
	}
	return &Claims{raw: raw, fields: fields}, nil
}

// Raw returns the claims set as the token carried it.
func (c *Claims) Raw() jsontext.Value {
	return slices.Clone(c.raw)
}

// MarshalJSON returns the claims set as the token carried it, so that
// claims can be logged or returned as they are.
func (c *Claims) MarshalJSON() ([]byte, error) {
	return slices.Clone(c.raw), nil
}

// As decodes the claims set into a T, typically a struct with a field for
// each claim the application reads.
func (c *Claims) As[T any]() (T, error) {
	var out T
	if err := json.Unmarshal(c.raw, &out); err != nil {
		return out, fmt.Errorf("auth: decode the claims into %T: %w", out, err)
	}
	return out, nil
}

// Has reports whether the claim is present, even when it is null.
func (c *Claims) Has(name string) bool {
	_, ok := c.fields[name]
	return ok
}

// Lookup returns the claim as raw JSON.
func (c *Claims) Lookup(name string) (jsontext.Value, bool) {
	v, ok := c.fields[name]
	if !ok {
		return nil, false
	}
	return slices.Clone(v), true
}

// Get returns the claim decoded as a JSON value: nil, bool, float64,
// string, []any or map[string]any.
func (c *Claims) Get(name string) (any, bool) {
	v, ok := c.fields[name]
	if !ok {
		return nil, false
	}
	var out any
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, false
	}
	return out, true
}

// GetString returns the claim when it is a JSON string.
func (c *Claims) GetString(name string) (string, bool) {
	return jsonString(c.fields[name])
}

// Issuer returns the iss claim.
func (c *Claims) Issuer() string {
	s, _ := c.GetString("iss")
	return s
}

// Subject returns the sub claim, the principal the token was issued to.
func (c *Claims) Subject() string {
	s, _ := c.GetString("sub")
	return s
}

// ID returns the jti claim.
func (c *Claims) ID() string {
	s, _ := c.GetString("jti")
	return s
}

// Audience returns the aud claim, which a token may carry as a string or as
// a list of strings.
func (c *Claims) Audience() []string {
	v := c.fields["aud"]
	if s, ok := jsonString(v); ok {
		return []string{s}
	}
	var list []string
	if err := json.Unmarshal(v, &list); err != nil {
		return nil
	}
	return list
}

// ExpiresAt returns the exp claim, the zero time when it is absent.
func (c *Claims) ExpiresAt() time.Time { return c.time("exp") }

// NotBefore returns the nbf claim, the zero time when it is absent.
func (c *Claims) NotBefore() time.Time { return c.time("nbf") }

// IssuedAt returns the iat claim, the zero time when it is absent.
func (c *Claims) IssuedAt() time.Time { return c.time("iat") }

// Scopes returns the scopes the token grants, read from the first of the
// scope, scp and scopes claims it carries: a space-separated string or a
// list of strings.
func (c *Claims) Scopes() []string {
	scopes, ok := c.scopes()
	if !ok {
		return nil
	}
	return scopes
}

func (c *Claims) time(name string) time.Time {
	seconds, ok := jsonNumber(c.fields[name])
	if !ok {
		return time.Time{}
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fraction*float64(time.Second)))
}

// scopes reads the granted scopes as Powertools does: the first present
// claim among scope, scp and scopes; a string splits on single spaces; a
// list must hold strings. ok is false when the claim has another shape, or
// holds something that is not a scope token.
func (c *Claims) scopes() (scopes []string, ok bool) {
	for _, name := range []string{"scope", "scp", "scopes"} {
		v, present := c.fields[name]
		if !present {
			continue
		}
		if s, isString := jsonString(v); isString {
			scopes = strings.FieldsFunc(s, func(r rune) bool { return r == ' ' })
		} else if err := json.Unmarshal(v, &scopes); err != nil || v.Kind() != '[' {
			return nil, false
		}
		break
	}
	for _, s := range scopes {
		if !authval.ScopeToken(s) {
			return nil, false
		}
	}
	return scopes, true
}

// jsonString decodes v when it is a JSON string.
func jsonString(v jsontext.Value) (string, bool) {
	if v.Kind() != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}

// jsonNumber decodes v when it is a finite JSON number. Numbers too large for
// a float64 count as infinite, as they do in Python.
func jsonNumber(v jsontext.Value) (float64, bool) {
	if v.Kind() != '0' {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// claimsKey is the context key Require stores the claims under.
type claimsKey struct{}

// ContextWithClaims returns a copy of ctx that carries claims. Require
// stores the verified claims this way for the handlers it protects; tests
// can do the same to call a handler directly.
func ContextWithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, claims)
}

// ClaimsFromContext returns the claims Require verified for the request
// whose context is ctx.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(claimsKey{}).(*Claims)
	return claims, ok && claims != nil
}
