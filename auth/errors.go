package auth

import "errors"

// Reason is a stable, credential-free reason for a failure, fit for logs and
// metrics. The values are Powertools Python's AuthFailureReason.
type Reason string

// The reasons a token or a request is rejected.
const (
	ReasonMissingToken      Reason = "missing_token"
	ReasonInvalidToken      Reason = "invalid_token"
	ReasonInvalidClaims     Reason = "invalid_claims"
	ReasonTokenExpired      Reason = "token_expired"
	ReasonInvalidSignature  Reason = "invalid_signature"
	ReasonInsufficientScope Reason = "insufficient_scope"
	ReasonForbidden         Reason = "forbidden"
	ReasonJWKSUnavailable   Reason = "jwks_unavailable"
)

// The sentinels an *Error matches with errors.Is. They form Powertools'
// hierarchy: every reason about the token itself matches ErrInvalidToken,
// and an insufficient scope matches ErrForbidden.
var (
	// ErrInvalidToken matches every failure that means the bearer token
	// cannot be trusted: missing, malformed, badly signed, expired, or with
	// claims that do not fit the profile.
	ErrInvalidToken = errors.New("invalid access token")
	// ErrMissingToken matches a request without an Authorization header.
	ErrMissingToken = errors.New("missing access token")
	// ErrInvalidClaims matches a missing or mismatching claim.
	ErrInvalidClaims = errors.New("invalid access token claims")
	// ErrTokenExpired matches a token past its exp claim and the clock
	// skew.
	ErrTokenExpired = errors.New("access token expired")
	// ErrInvalidSignature matches a signature the signing key rejects.
	ErrInvalidSignature = errors.New("invalid access token signature")
	// ErrForbidden matches a verified caller who lacks a permission.
	ErrForbidden = errors.New("forbidden")
	// ErrInsufficientScope matches a verified caller without a required
	// scope.
	ErrInsufficientScope = errors.New("insufficient scope")
	// ErrJWKSFetch matches signing keys that could not be fetched: the
	// identity provider is unreachable, slow, or answered something
	// unusable. It is the one retryable failure.
	ErrJWKSFetch = errors.New("unable to retrieve verification keys")

	// ErrInvalidConfig matches a configuration the package refuses: an
	// issuer that is not an HTTPS URL, a symmetric algorithm, a scope that
	// is not a scope token. The message names the problem, never the value.
	ErrInvalidConfig = errors.New("invalid auth configuration")
	// ErrInvalidEvent matches an authorizer event of the wrong type or
	// version, or whose method or route ARN is not concrete.
	ErrInvalidEvent = errors.New("invalid authorizer event")
)

// Error is an authentication or authorization failure. Its message is fixed
// for its Reason and never carries the token, its claims or the identity
// provider's answer, so that it is safe to log; the cause of a failure is
// deliberately dropped.
type Error struct {
	// Reason says why the request failed.
	Reason Reason
	// Retryable reports whether a later attempt may succeed, which is only
	// the case when the signing keys were unavailable.
	Retryable bool
}

func (e *Error) Error() string {
	return e.sentinel().Error()
}

// Is matches the sentinels of the reason and of its parents in the
// hierarchy.
func (e *Error) Is(target error) bool {
	switch target {
	case e.sentinel():
		return true
	case ErrInvalidToken:
		return e.Reason.invalidToken()
	case ErrForbidden:
		return e.Reason == ReasonInsufficientScope
	default:
		return false
	}
}

func (e *Error) sentinel() error {
	switch e.Reason {
	case ReasonMissingToken:
		return ErrMissingToken
	case ReasonInvalidClaims:
		return ErrInvalidClaims
	case ReasonTokenExpired:
		return ErrTokenExpired
	case ReasonInvalidSignature:
		return ErrInvalidSignature
	case ReasonForbidden:
		return ErrForbidden
	case ReasonInsufficientScope:
		return ErrInsufficientScope
	case ReasonJWKSUnavailable:
		return ErrJWKSFetch
	default:
		return ErrInvalidToken
	}
}

// invalidToken reports whether the reason is about the token itself, which
// HTTP answers with 401.
func (r Reason) invalidToken() bool {
	switch r {
	case ReasonMissingToken, ReasonInvalidToken, ReasonInvalidClaims, ReasonTokenExpired, ReasonInvalidSignature:
		return true
	default:
		return false
	}
}

// The errors every failure path returns. Each call builds a new value, so
// that a caller changing one never affects another.
func errInvalidToken() error     { return &Error{Reason: ReasonInvalidToken} }
func errMissingToken() error     { return &Error{Reason: ReasonMissingToken} }
func errInvalidClaims() error    { return &Error{Reason: ReasonInvalidClaims} }
func errTokenExpired() error     { return &Error{Reason: ReasonTokenExpired} }
func errInvalidSignature() error { return &Error{Reason: ReasonInvalidSignature} }
func errForbidden() error        { return &Error{Reason: ReasonForbidden} }
func errInsufficientScope() error {
	return &Error{Reason: ReasonInsufficientScope}
}
func errJWKSFetch() error { return &Error{Reason: ReasonJWKSUnavailable, Retryable: true} }
