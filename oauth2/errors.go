package oauth2

import "errors"

// Reason is a stable, credential-free reason for a failure, fit for logs and
// metrics. The values are those of Powertools Python's AuthFailureReason.
type Reason string

// The reasons an acquisition or a request fails.
const (
	ReasonTokenExchangeFailed     Reason = "token_exchange_failed"
	ReasonDownstreamRequestFailed Reason = "downstream_request_failed"
)

var (
	// ErrTokenExchange matches a token that could not be acquired: the
	// secret source failed, the token endpoint refused the client or
	// answered something unusable, or the budget ran out. Retryable tells
	// a transient failure from a permanent one.
	ErrTokenExchange = errors.New("unable to acquire an access token")
	// ErrDownstreamRequest matches an authenticated request whose transport
	// failed. The server may have performed it already, so it is never
	// retryable: whether to replay it is the caller's call.
	ErrDownstreamRequest = errors.New("authenticated request failed")

	// ErrInvalidConfig matches a configuration the client refuses. The
	// message names the problem, never the value.
	ErrInvalidConfig = errors.New("invalid OAuth2 configuration")
	// ErrInvalidRequest matches a request the authenticated transport
	// refuses before acquiring a token: not HTTPS, an invalid method or
	// header, or an Authorization header of its own.
	ErrInvalidRequest = errors.New("invalid authenticated request")
)

// Error is a failure to acquire a token or to send an authenticated
// request. Its message is fixed for its Reason and never carries the client
// secret, the token or the server's answer; the cause is deliberately
// dropped.
type Error struct {
	// Reason says what failed.
	Reason Reason
	// Retryable reports whether a later attempt may succeed: a transient
	// failure of the token endpoint, or a budget that ran out.
	Retryable bool
}

func (e *Error) Error() string {
	return e.sentinel().Error()
}

// Is matches the sentinel of the reason.
func (e *Error) Is(target error) bool {
	return target == e.sentinel()
}

func (e *Error) sentinel() error {
	if e.Reason == ReasonDownstreamRequestFailed {
		return ErrDownstreamRequest
	}
	return ErrTokenExchange
}

func errTokenExchange(retryable bool) *Error {
	return &Error{Reason: ReasonTokenExchangeFailed, Retryable: retryable}
}

func errDownstream() *Error {
	return &Error{Reason: ReasonDownstreamRequestFailed}
}
