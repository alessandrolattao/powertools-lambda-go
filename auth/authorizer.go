package auth

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
)

// Values of API Gateway authorizer events and responses.
const (
	eventTypeToken   = "TOKEN"
	eventTypeRequest = "REQUEST"
	payloadV2        = "2.0"
	policyVersion    = "2012-10-17"
	invokeAction     = "execute-api:Invoke"
	unauthorized     = "unauthorized"
	maxARNLength     = 512
)

// concreteARN is the shape of a method or route ARN: one stage, one method,
// one resource.
var concreteARN = regexp.MustCompile(`^arn:[a-z0-9-]+:execute-api:[a-z0-9-]+:[0-9]{12}:[a-z0-9]+/[^/]+/[A-Z]+/.*$`)

type authorizerOptionFunc func(c *authorizerConfig)

func (f authorizerOptionFunc) applyAuthorizer(c *authorizerConfig) { f(c) }

// WithContextClaims copies the named claims into the authorizer's context,
// where the integration reads them. Only claims that are strings, booleans
// or finite numbers are copied; others are skipped, as are missing ones.
// Nothing is copied by default, and "claims" is reserved by API Gateway.
func WithContextClaims(names ...string) AuthorizerOption {
	return authorizerOptionFunc(func(c *authorizerConfig) { c.contextClaims = append(c.contextClaims, names...) })
}

// WithOnError calls record with every authentication failure, for logs and
// metrics: it cannot change the outcome, which is a Deny for invalid
// credentials and an error for unavailable keys. Nothing is logged unless it
// does so.
func WithOnError(record func(ctx context.Context, err *Error)) AuthorizerOption {
	return authorizerOptionFunc(func(c *authorizerConfig) { c.onError = record })
}

type authorizerConfig struct {
	scopes        []string
	contextClaims []string
	onError       func(ctx context.Context, err *Error)
}

func newAuthorizerConfig(opts []AuthorizerOption) (*authorizerConfig, error) {
	cfg := &authorizerConfig{}
	for _, opt := range opts {
		opt.applyAuthorizer(cfg)
	}
	var ok bool
	if cfg.scopes, ok = authval.Scopes(cfg.scopes); !ok {
		return nil, fmt.Errorf("%w: scopes must be valid OAuth scope tokens", ErrInvalidConfig)
	}
	if cfg.contextClaims, ok = authval.Strings(cfg.contextClaims); !ok {
		return nil, fmt.Errorf("%w: context claim names must not be blank", ErrInvalidConfig)
	}
	if slices.Contains(cfg.contextClaims, "claims") {
		return nil, fmt.Errorf("%w: claims is reserved in an API Gateway authorizer context", ErrInvalidConfig)
	}
	return cfg, nil
}

// AuthorizeToken answers a REST API TOKEN authorizer: an IAM policy that
// allows the invoked method when the token in authorizationToken is valid
// and grants the required scopes, and denies it otherwise. The principal is
// the token's sub claim, which an Allow requires.
//
// Unavailable signing keys return an error matching ErrJWKSFetch, so that
// API Gateway answers 5xx instead of caching a Deny. An event of another type,
// or whose method ARN is not concrete, matches ErrInvalidEvent.
//
// Disable API Gateway's authorizer caching, or key it carefully: a cached
// Allow outlives the token's expiry until its TTL ends.
func AuthorizeToken(ctx context.Context, v Verifier, event events.APIGatewayCustomAuthorizerRequest, opts ...AuthorizerOption) (events.APIGatewayCustomAuthorizerResponse, error) {
	token := func() (string, error) {
		return bearerToken(event.AuthorizationToken, event.AuthorizationToken != "")
	}
	return restAuthorize(ctx, v, event.Type, eventTypeToken, event.MethodArn, token, opts)
}

// AuthorizeRequest answers a REST API REQUEST authorizer with an IAM
// policy, the token coming from the Authorization header. It works like
// AuthorizeToken.
func AuthorizeRequest(ctx context.Context, v Verifier, event events.APIGatewayCustomAuthorizerRequestTypeRequest, opts ...AuthorizerOption) (events.APIGatewayCustomAuthorizerResponse, error) {
	token := func() (string, error) { return eventToken(event.Headers, event.MultiValueHeaders) }
	return restAuthorize(ctx, v, event.Type, eventTypeRequest, event.MethodArn, token, opts)
}

// AuthorizeHTTPV1 answers an HTTP API authorizer with payload format 1.0,
// which takes an IAM policy. It works like AuthorizeRequest.
func AuthorizeHTTPV1(ctx context.Context, v Verifier, event events.APIGatewayV2CustomAuthorizerV1Request, opts ...AuthorizerOption) (events.APIGatewayCustomAuthorizerResponse, error) {
	if event.Version == payloadV2 {
		return events.APIGatewayCustomAuthorizerResponse{}, fmt.Errorf("%w: a payload 2.0 event names a route ARN, not a method ARN", ErrInvalidEvent)
	}
	token := func() (string, error) { return eventToken(event.Headers, nil) }
	return restAuthorize(ctx, v, event.Type, eventTypeRequest, event.MethodArn, token, opts)
}

// AuthorizeHTTP answers an HTTP API authorizer with payload format 2.0
// with an IAM policy for the invoked route. It works like AuthorizeRequest.
func AuthorizeHTTP(ctx context.Context, v Verifier, event events.APIGatewayV2CustomAuthorizerV2Request, opts ...AuthorizerOption) (events.APIGatewayV2CustomAuthorizerIAMPolicyResponse, error) {
	var out events.APIGatewayV2CustomAuthorizerIAMPolicyResponse
	if err := httpEvent(event); err != nil {
		return out, err
	}
	arn, err := validARN(event.RouteArn)
	if err != nil {
		return out, err
	}
	claims, allowed, cfg, err := authorize(ctx, v, opts, true, func() (string, error) { return eventToken(event.Headers, nil) })
	if err != nil {
		return out, err
	}
	out.PrincipalID, out.PolicyDocument, out.Context = iamPolicy(claims, allowed, arn, cfg)
	return out, nil
}

// AuthorizeHTTPSimple answers an HTTP API authorizer with payload format
// 2.0 with a simple response, which the authorizer must have enabled:
// isAuthorized true when the token is valid and grants the required scopes.
// It requires no sub claim.
func AuthorizeHTTPSimple(ctx context.Context, v Verifier, event events.APIGatewayV2CustomAuthorizerV2Request, opts ...AuthorizerOption) (events.APIGatewayV2CustomAuthorizerSimpleResponse, error) {
	var out events.APIGatewayV2CustomAuthorizerSimpleResponse
	if err := httpEvent(event); err != nil {
		return out, err
	}
	claims, allowed, cfg, err := authorize(ctx, v, opts, false, func() (string, error) { return eventToken(event.Headers, nil) })
	if err != nil {
		return out, err
	}
	out.IsAuthorized = allowed
	if allowed {
		out.Context = cfg.context(claims)
	}
	return out, nil
}

func httpEvent(event events.APIGatewayV2CustomAuthorizerV2Request) error {
	if event.Type != eventTypeRequest || event.Version != payloadV2 {
		return fmt.Errorf("%w: an HTTP API REQUEST authorizer event with payload version 2.0 is required", ErrInvalidEvent)
	}
	return nil
}

// restAuthorize is the IAM answer of the authorizers that name the method
// ARN.
func restAuthorize(ctx context.Context, v Verifier, got, want, methodARN string, token func() (string, error), opts []AuthorizerOption) (events.APIGatewayCustomAuthorizerResponse, error) {
	var out events.APIGatewayCustomAuthorizerResponse
	if got != want {
		return out, fmt.Errorf("%w: an API Gateway %s authorizer event is required", ErrInvalidEvent, want)
	}
	arn, err := validARN(methodARN)
	if err != nil {
		return out, err
	}
	claims, allowed, cfg, err := authorize(ctx, v, opts, true, token)
	if err != nil {
		return out, err
	}
	out.PrincipalID, out.PolicyDocument, out.Context = iamPolicy(claims, allowed, arn, cfg)
	return out, nil
}

// validARN accepts a concrete method or route ARN only: an Allow must never
// grant more than the invoked resource.
func validARN(arn string) (string, error) {
	if utf8.RuneCountInString(arn) > maxARNLength || !concreteARN.MatchString(arn) || strings.ContainsAny(arn, "*?\r\n") {
		return "", fmt.Errorf("%w: a concrete API Gateway method or route ARN of at most 512 characters is required", ErrInvalidEvent)
	}
	return arn, nil
}

// authorize verifies the token of an authorizer event. allowed is false for
// invalid credentials and insufficient permissions, which deny; unavailable
// keys and errors from a custom Verifier are returned, so that the
// invocation fails.
func authorize(ctx context.Context, v Verifier, opts []AuthorizerOption, principal bool, token func() (string, error)) (claims *Claims, allowed bool, cfg *authorizerConfig, err error) {
	if v == nil {
		return nil, false, nil, fmt.Errorf("%w: a verifier is required", ErrInvalidConfig)
	}
	if cfg, err = newAuthorizerConfig(opts); err != nil {
		return nil, false, nil, err
	}
	claims, err = verifyForAuthorizer(ctx, v, cfg, principal, token)
	if err == nil {
		return claims, true, cfg, nil
	}
	authErr, ok := errors.AsType[*Error](err)
	if !ok {
		return nil, false, nil, err
	}
	if cfg.onError != nil {
		cfg.onError(ctx, authErr)
	}
	if errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrForbidden) {
		return nil, false, cfg, nil
	}
	return nil, false, nil, err
}

func verifyForAuthorizer(ctx context.Context, v Verifier, cfg *authorizerConfig, principal bool, token func() (string, error)) (*Claims, error) {
	raw, err := token()
	if err != nil {
		return nil, err
	}
	claims, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := enforceScopes(claims, cfg.scopes); err != nil {
		return nil, err
	}
	if principal && strings.TrimSpace(claims.Subject()) == "" {
		// An IAM policy names its principal: a token without one is not
		// fit for it.
		return nil, errInvalidClaims()
	}
	return claims, nil
}

// iamPolicy is the IAM answer: Allow or Deny on the exact invoked ARN,
// never a wildcard.
func iamPolicy(claims *Claims, allowed bool, arn string, cfg *authorizerConfig) (principal string, policy events.APIGatewayCustomAuthorizerPolicy, authContext map[string]any) {
	principal, effect := unauthorized, "Deny"
	if allowed {
		principal, effect = claims.Subject(), "Allow"
		authContext = cfg.context(claims)
	}
	policy = events.APIGatewayCustomAuthorizerPolicy{
		Version: policyVersion,
		Statement: []events.IAMPolicyStatement{{
			Action:   []string{invokeAction},
			Effect:   effect,
			Resource: []string{arn},
		}},
	}
	return principal, policy, authContext
}

// context copies the selected scalar claims; it is nil when none is
// copied, so that the response carries no context at all.
func (c *authorizerConfig) context(claims *Claims) map[string]any {
	var out map[string]any
	for _, name := range c.contextClaims {
		value, ok := claims.fields[name]
		if !ok {
			continue
		}
		scalar, ok := scalarClaim(value)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(c.contextClaims))
		}
		out[name] = scalar
	}
	return out
}

// scalarClaim decodes a string, a boolean or a finite number; integers stay
// integers.
func scalarClaim(v jsontext.Value) (any, bool) {
	switch v.Kind() {
	case '"':
		return jsonString(v)
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, false
		}
		return b, true
	case '0':
		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return n, true
		}
		return jsonNumber(v)
	default:
		return nil, false
	}
}
