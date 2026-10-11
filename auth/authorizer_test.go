package auth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	methodARN = "arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/orders/42"
	routeARN  = "arn:aws:execute-api:eu-west-1:123456789012:abc123/$default/GET/orders"
	allClaims = `{"sub":"user-1","scope":"orders:read","tenant":"t1","admin":true,"level":3,` +
		`"ratio":0.5,"big":12345678901234567890,"nested":{"a":1},"list":[1],"none":null}`
)

func tokenEvent(token string) events.APIGatewayCustomAuthorizerRequest {
	return events.APIGatewayCustomAuthorizerRequest{Type: "TOKEN", AuthorizationToken: token, MethodArn: methodARN}
}

func TestAuthorizeToken(t *testing.T) {
	t.Parallel()
	v := claimsVerifier(t, allClaims, errInvalidSignature())

	out, err := AuthorizeToken(t.Context(), v, tokenEvent("Bearer good"),
		WithScopes("orders:read"), WithContextClaims("sub", "tenant", "admin", "level", "ratio", "big", "nested", "list", "none", "missing"))
	require.NoError(t, err)
	assert.Equal(t, "user-1", out.PrincipalID)
	assert.Equal(t, policyFor("Allow", methodARN), out.PolicyDocument)
	assert.Equal(t, map[string]any{
		"sub": "user-1", "tenant": "t1", "admin": true, "level": int64(3), "ratio": 0.5, "big": 12345678901234567890.0,
	}, out.Context)

	out, err = AuthorizeToken(t.Context(), v, tokenEvent("Bearer good"))
	require.NoError(t, err)
	assert.Nil(t, out.Context, "no claim is copied by default")

	for name, token := range map[string]string{"invalid": "Bearer bad", "missing": "", "wrong scheme": "Basic good"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := AuthorizeToken(t.Context(), v, tokenEvent(token), WithContextClaims("sub"))
			require.NoError(t, err)
			assert.Equal(t, "unauthorized", out.PrincipalID)
			assert.Equal(t, policyFor("Deny", methodARN), out.PolicyDocument)
			assert.Nil(t, out.Context)
		})
	}

	t.Run("insufficient scope denies", func(t *testing.T) {
		t.Parallel()
		out, err := AuthorizeToken(t.Context(), v, tokenEvent("Bearer good"), WithScopes("orders:write"))
		require.NoError(t, err)
		assert.Equal(t, "Deny", out.PolicyDocument.Statement[0].Effect)
	})
}

func TestAuthorizeRequiresAPrincipal(t *testing.T) {
	t.Parallel()
	for name, claims := range map[string]string{"no sub": `{}`, "blank sub": `{"sub":"  "}`, "sub number": `{"sub":1}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var recorded *Error
			out, err := AuthorizeToken(t.Context(), claimsVerifier(t, claims, nil), tokenEvent("Bearer good"),
				WithOnError(func(_ context.Context, err *Error) { recorded = err }))
			require.NoError(t, err)
			assert.Equal(t, "Deny", out.PolicyDocument.Statement[0].Effect)
			require.NotNil(t, recorded)
			assert.Equal(t, ReasonInvalidClaims, recorded.Reason)
		})
	}

	t.Run("simple responses need none", func(t *testing.T) {
		t.Parallel()
		out, err := AuthorizeHTTPSimple(t.Context(), claimsVerifier(t, `{}`, nil), httpEvent2("Bearer good"))
		require.NoError(t, err)
		assert.True(t, out.IsAuthorized)
	})
}

func TestAuthorizeKeysUnavailable(t *testing.T) {
	t.Parallel()
	var recorded []Reason
	onError := WithOnError(func(_ context.Context, err *Error) { recorded = append(recorded, err.Reason) })
	v := claimsVerifier(t, allClaims, errJWKSFetch())

	_, err := AuthorizeToken(t.Context(), v, tokenEvent("Bearer bad"), onError)
	require.ErrorIs(t, err, ErrJWKSFetch, "API Gateway must answer 5xx, not cache a Deny")
	_, err = AuthorizeHTTPSimple(t.Context(), v, httpEvent2("Bearer bad"), onError)
	require.ErrorIs(t, err, ErrJWKSFetch)
	assert.Equal(t, []Reason{ReasonJWKSUnavailable, ReasonJWKSUnavailable}, recorded)

	boom := errors.New("boom")
	_, err = AuthorizeToken(t.Context(), claimsVerifier(t, allClaims, boom), tokenEvent("Bearer bad"), onError)
	require.ErrorIs(t, err, boom, "errors of a custom verifier propagate")
	assert.Len(t, recorded, 2, "and are not auth failures")
}

func TestAuthorizeRequest(t *testing.T) {
	t.Parallel()
	v := claimsVerifier(t, allClaims, errInvalidToken())
	event := events.APIGatewayCustomAuthorizerRequestTypeRequest{
		Type: "REQUEST", MethodArn: methodARN,
		Headers:           map[string]string{"authorization": "Bearer good"},
		MultiValueHeaders: map[string][]string{"Authorization": {"Bearer good"}},
	}
	out, err := AuthorizeRequest(t.Context(), v, event, WithContextClaims("tenant"))
	require.NoError(t, err)
	assert.Equal(t, policyFor("Allow", methodARN), out.PolicyDocument)
	assert.Equal(t, map[string]any{"tenant": "t1"}, out.Context)

	event.MultiValueHeaders = map[string][]string{"Authorization": {"Bearer other"}}
	out, err = AuthorizeRequest(t.Context(), v, event)
	require.NoError(t, err)
	assert.Equal(t, policyFor("Deny", methodARN), out.PolicyDocument)

	v1 := events.APIGatewayV2CustomAuthorizerV1Request{
		Version: "1.0", Type: "REQUEST", MethodArn: methodARN, Headers: map[string]string{"Authorization": "Bearer good"},
	}
	out, err = AuthorizeHTTPV1(t.Context(), v, v1)
	require.NoError(t, err)
	assert.Equal(t, "user-1", out.PrincipalID)
	assert.Equal(t, policyFor("Allow", methodARN), out.PolicyDocument)
}

func httpEvent2(authorization string) events.APIGatewayV2CustomAuthorizerV2Request {
	headers := map[string]string{}
	if authorization != "" {
		headers["authorization"] = authorization
	}
	return events.APIGatewayV2CustomAuthorizerV2Request{Version: "2.0", Type: "REQUEST", RouteArn: routeARN, Headers: headers}
}

func TestAuthorizeHTTP(t *testing.T) {
	t.Parallel()
	v := claimsVerifier(t, allClaims, errInvalidToken())

	iam, err := AuthorizeHTTP(t.Context(), v, httpEvent2("Bearer good"), WithScopes("orders:read"), WithContextClaims("sub"))
	require.NoError(t, err)
	assert.Equal(t, "user-1", iam.PrincipalID)
	assert.Equal(t, policyFor("Allow", routeARN), iam.PolicyDocument)
	assert.Equal(t, map[string]any{"sub": "user-1"}, iam.Context)

	iam, err = AuthorizeHTTP(t.Context(), v, httpEvent2(""))
	require.NoError(t, err)
	assert.Equal(t, policyFor("Deny", routeARN), iam.PolicyDocument)

	simple, err := AuthorizeHTTPSimple(t.Context(), v, httpEvent2("Bearer good"), WithContextClaims("tenant"))
	require.NoError(t, err)
	assert.Equal(t, events.APIGatewayV2CustomAuthorizerSimpleResponse{IsAuthorized: true, Context: map[string]any{"tenant": "t1"}}, simple)

	simple, err = AuthorizeHTTPSimple(t.Context(), v, httpEvent2("Bearer bad"), WithContextClaims("tenant"))
	require.NoError(t, err)
	assert.Equal(t, events.APIGatewayV2CustomAuthorizerSimpleResponse{IsAuthorized: false}, simple)

	raw, err := json.Marshal(simple)
	require.NoError(t, err)
	assert.JSONEq(t, `{"isAuthorized":false}`, string(raw), "the context is left out")
}

func TestAuthorizeRefusesEvents(t *testing.T) {
	t.Parallel()
	v := claimsVerifier(t, allClaims, nil)
	ctx := t.Context()
	wrongVersion := httpEvent2("Bearer good")
	wrongVersion.Version = "1.0"
	wrongType := httpEvent2("Bearer good")
	wrongType.Type = "TOKEN"
	badRoute := httpEvent2("Bearer good")
	badRoute.RouteArn = "arn:aws:execute-api:eu-west-1:123456789012:abc123/*/*"

	cases := map[string]func() error{
		"TOKEN handler, REQUEST event": func() error {
			_, err := AuthorizeToken(ctx, v, events.APIGatewayCustomAuthorizerRequest{Type: "REQUEST", MethodArn: methodARN})
			return err
		},
		"REQUEST handler, TOKEN event": func() error {
			_, err := AuthorizeRequest(ctx, v, events.APIGatewayCustomAuthorizerRequestTypeRequest{Type: "TOKEN", MethodArn: methodARN})
			return err
		},
		"HTTP v1 handler, 2.0 event": func() error {
			_, err := AuthorizeHTTPV1(ctx, v, events.APIGatewayV2CustomAuthorizerV1Request{Version: "2.0", Type: "REQUEST", MethodArn: methodARN})
			return err
		},
		"HTTP IAM, payload 1.0":    func() error { _, err := AuthorizeHTTP(ctx, v, wrongVersion); return err },
		"simple, payload 1.0":      func() error { _, err := AuthorizeHTTPSimple(ctx, v, wrongVersion); return err },
		"simple, TOKEN":            func() error { _, err := AuthorizeHTTPSimple(ctx, v, wrongType); return err },
		"HTTP IAM, wildcard route": func() error { _, err := AuthorizeHTTP(ctx, v, badRoute); return err },
	}
	for _, arn := range []string{
		"",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/*/orders",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/orders/*",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/orders?x",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/orders\r\n",
		"arn:aws:lambda:eu-west-1:123456789012:abc123/prod/GET/orders",
		"arn:aws:execute-api:eu-west-1:12345678901:abc123/prod/GET/orders",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/get/orders",
		"arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/" + strings.Repeat("a", 512),
	} {
		cases["method ARN "+arn] = func() error {
			_, err := AuthorizeToken(ctx, v, events.APIGatewayCustomAuthorizerRequest{Type: "TOKEN", AuthorizationToken: "Bearer good", MethodArn: arn})
			return err
		}
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, call(), ErrInvalidEvent)
		})
	}

	for name, opt := range map[string]AuthorizerOption{
		"reserved claims": WithContextClaims("sub", "claims"),
		"blank claim":     WithContextClaims(""),
		"invalid scope":   WithScopes("a b"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := AuthorizeToken(ctx, v, tokenEvent("Bearer good"), opt)
			require.ErrorIs(t, err, ErrInvalidConfig)
		})
	}

	t.Run("the longest concrete ARN", func(t *testing.T) {
		t.Parallel()
		prefix := "arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/"
		_, err := AuthorizeToken(ctx, v, events.APIGatewayCustomAuthorizerRequest{
			Type: "TOKEN", AuthorizationToken: "Bearer good", MethodArn: prefix + strings.Repeat("a", 512-len(prefix)),
		})
		require.NoError(t, err)
	})
}

func policyFor(effect, arn string) events.APIGatewayCustomAuthorizerPolicy {
	return events.APIGatewayCustomAuthorizerPolicy{
		Version: "2012-10-17",
		Statement: []events.IAMPolicyStatement{{
			Action:   []string{"execute-api:Invoke"},
			Effect:   effect,
			Resource: []string{arn},
		}},
	}
}
