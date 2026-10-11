package auth

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// authorizationHeader is the header bearer tokens travel in, matched in any
// case.
const authorizationHeader = "authorization"

// VerifyAuthorizationHeader verifies the token of an HTTP Authorization
// header value, "Bearer <token>" with the scheme in any case. An empty value
// is a missing header (ErrMissingToken); any other scheme or shape is an
// invalid token.
func VerifyAuthorizationHeader(ctx context.Context, v Verifier, value string) (*Claims, error) {
	token, err := bearerToken(value, value != "")
	if err != nil {
		return nil, err
	}
	return v.Verify(ctx, token)
}

// VerifyRequest verifies the bearer token of r's Authorization header, with
// r's context. A request without the header fails with ErrMissingToken; one
// with more than one Authorization value is invalid, since nobody can tell
// which one a proxy in front of the function has checked.
//
// It is the building block of Require, for handlers that authorize in
// their own way.
func VerifyRequest(v Verifier, r *http.Request) (*Claims, error) {
	var values []string
	for name, vs := range r.Header {
		if strings.EqualFold(name, authorizationHeader) {
			values = append(values, vs...)
		}
	}
	var token string
	var err error
	switch len(values) {
	case 0:
		token, err = bearerToken("", false)
	case 1:
		token, err = bearerToken(values[0], true)
	default:
		err = errInvalidToken()
	}
	if err != nil {
		return nil, err
	}
	return v.Verify(r.Context(), token)
}

// bearerToken extracts the token of an Authorization header value: two
// whitespace-separated parts, the first one "Bearer" in any case.
func bearerToken(value string, present bool) (string, error) {
	if !present {
		return "", errMissingToken()
	}
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", errInvalidToken()
	}
	return parts[1], nil
}

// eventToken extracts the bearer token of an API Gateway event's headers,
// as Powertools does: the header name matches in any case and only once;
// a multi-value header must hold exactly one value, the same as the
// single-value header when the event has both.
func eventToken(headers map[string]string, multi map[string][]string) (string, error) {
	var values []string
	for name, value := range headers {
		if strings.EqualFold(name, authorizationHeader) {
			values = append(values, value)
		}
	}
	var multiValues [][]string
	for name, entries := range multi {
		if strings.EqualFold(name, authorizationHeader) {
			multiValues = append(multiValues, entries)
		}
	}
	if len(values) > 1 || len(multiValues) > 1 {
		return "", errInvalidToken()
	}
	if len(multiValues) == 1 {
		entries := multiValues[0]
		if len(entries) != 1 || (len(values) == 1 && values[0] != entries[0]) {
			return "", errInvalidToken()
		}
		return bearerToken(entries[0], true)
	}
	if len(values) == 1 {
		return bearerToken(values[0], true)
	}
	return bearerToken("", false)
}

// enforceScopes checks that the claims grant every required scope. Claims
// whose scopes are not scope tokens are invalid, even when no scope is
// required, as in Powertools.
func enforceScopes(c *Claims, required []string) error {
	granted, ok := c.scopes()
	if !ok {
		return errInvalidClaims()
	}
	for _, scope := range required {
		if !slices.Contains(granted, scope) {
			return errInsufficientScope()
		}
	}
	return nil
}
