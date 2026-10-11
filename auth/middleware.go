package auth

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Failure describes a request Require refuses: the status and headers of
// the default response, and why. It carries nothing from the token, so a
// custom response or a log line built from it leaks nothing.
type Failure struct {
	// StatusCode is 401 for a missing or invalid token, 403 for a missing
	// scope or a refused custom authorization, 503 when the signing keys
	// are unavailable.
	StatusCode int
	// Header holds the WWW-Authenticate challenge RFC 6750 prescribes for
	// the failure, if any. Keep it in a custom response.
	Header http.Header
	// Reason is the stable reason of the failure.
	Reason Reason
	// Retryable reports whether the same request may succeed later.
	Retryable bool
}

// RequireOption configures Require.
type RequireOption interface {
	applyRequire(c *requireConfig)
}

// AuthorizerOption configures the Authorize functions.
type AuthorizerOption interface {
	applyAuthorizer(c *authorizerConfig)
}

// ScopeOption is the option WithScopes returns: it configures Require and
// the Authorize functions alike.
type ScopeOption []string

func (o ScopeOption) applyRequire(c *requireConfig)       { c.scopes = append(c.scopes, o...) }
func (o ScopeOption) applyAuthorizer(c *authorizerConfig) { c.scopes = append(c.scopes, o...) }

// WithScopes requires every one of scopes in the token, read from its
// scope, scp or scopes claim. Each must be an OAuth scope token.
func WithScopes(scopes ...string) ScopeOption {
	return ScopeOption(scopes)
}

type requireOptionFunc func(c *requireConfig)

func (f requireOptionFunc) applyRequire(c *requireConfig) { f(c) }

// WithAuthorize adds a policy of your own, run once the token is verified
// and the scopes are checked: the request goes on only when allow returns
// true, and is otherwise refused with 403.
func WithAuthorize(allow func(r *http.Request, claims *Claims) bool) RequireOption {
	return requireOptionFunc(func(c *requireConfig) { c.authorize = allow })
}

// WithErrorResponse writes the response of a refused request in place of
// the default JSON {"message": "Unauthorized"} (or Forbidden, or Service
// Unavailable). It can log or count the failure, but the protected handler
// never runs. Nothing is logged unless it does so.
func WithErrorResponse(respond func(w http.ResponseWriter, r *http.Request, f Failure)) RequireOption {
	return requireOptionFunc(func(c *requireConfig) { c.respond = respond })
}

type requireConfig struct {
	scopes    []string
	authorize func(r *http.Request, claims *Claims) bool
	respond   func(w http.ResponseWriter, r *http.Request, f Failure)
}

// Require returns a net/http middleware that lets a request through only
// with a valid bearer token granting every required scope: Powertools'
// Event Handler require(). The verified claims travel in the request's
// context, where ClaimsFromContext finds them; they never outlive the
// request.
//
// A missing or invalid token is answered with 401, a missing scope or a
// refused authorization with 403, unavailable signing keys with 503, all
// with the WWW-Authenticate header of RFC 6750. An error from a Verifier
// that is not an *Error is answered with 500.
//
// An invalid scope fails with ErrInvalidConfig.
func Require(v Verifier, opts ...RequireOption) (func(http.Handler) http.Handler, error) {
	var cfg requireConfig
	for _, opt := range opts {
		opt.applyRequire(&cfg)
	}
	scopes, ok := authval.Scopes(cfg.scopes)
	if !ok {
		return nil, fmt.Errorf("%w: scopes must be valid OAuth scope tokens", ErrInvalidConfig)
	}
	cfg.scopes = scopes
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := cfg.check(v, r)
			if err != nil {
				cfg.refuse(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithClaims(r.Context(), claims)))
		})
	}, nil
}

func (c *requireConfig) check(v Verifier, r *http.Request) (*Claims, error) {
	claims, err := VerifyRequest(v, r)
	if err != nil {
		return nil, err
	}
	if err := enforceScopes(claims, c.scopes); err != nil {
		return nil, err
	}
	if c.authorize != nil && !c.authorize(r, claims) {
		return nil, errForbidden()
	}
	return claims, nil
}

func (c *requireConfig) refuse(w http.ResponseWriter, r *http.Request, err error) {
	authErr, ok := errors.AsType[*Error](err)
	if !ok {
		debug.Logger().DebugContext(r.Context(), "auth: the verifier failed", "err", err)
		writeMessage(w, r, http.StatusInternalServerError, nil)
		return
	}
	f := c.failure(authErr)
	if c.respond != nil {
		c.respond(w, r, f)
		return
	}
	writeMessage(w, r, f.StatusCode, f.Header)
}

// failure maps an error to its response, as Powertools does.
func (c *requireConfig) failure(err *Error) Failure {
	f := Failure{Header: http.Header{}, Reason: err.Reason, Retryable: err.Retryable}
	switch {
	case errors.Is(err, ErrMissingToken):
		f.StatusCode = http.StatusUnauthorized
		f.Header.Set("WWW-Authenticate", "Bearer")
	case errors.Is(err, ErrInvalidToken):
		f.StatusCode = http.StatusUnauthorized
		f.Header.Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	case errors.Is(err, ErrInsufficientScope):
		f.StatusCode = http.StatusForbidden
		f.Header.Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+strings.Join(c.scopes, " ")+`"`)
	case errors.Is(err, ErrForbidden):
		f.StatusCode = http.StatusForbidden
	default:
		f.StatusCode = http.StatusServiceUnavailable
	}
	return f
}

// message is the body of the default responses.
type message struct {
	Message string `json:"message"`
}

func writeMessage(w http.ResponseWriter, r *http.Request, status int, header http.Header) {
	maps.Copy(w.Header(), header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, err := json.Marshal(message{Message: http.StatusText(status)})
	if err == nil {
		_, err = w.Write(body)
	}
	if err != nil {
		debug.Logger().DebugContext(r.Context(), "auth: writing the response failed", "err", err)
	}
}
