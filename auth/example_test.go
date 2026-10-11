package auth_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwk"
	"github.com/lestrrat-go/jwx/v4/jws"

	"github.com/alessandrolattao/powertools-lambda-go/auth"
)

const (
	issuer   = "https://idp.example.com/"
	resource = "https://orders.example.com"
)

// identityProvider plays the identity provider of the examples: it returns
// the key set it publishes and an access token it issued.
func identityProvider() (jwks []byte, token string) {
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	public, err := jwk.Import[jwk.Key](private.Public())
	must(err)
	must(public.Set(jwk.KeyIDKey, "key-1"))
	set := jwk.NewSet()
	must(set.AddKey(public))
	jwks, err = json.Marshal(set)
	must(err)

	claims, err := json.Marshal(map[string]any{
		"iss":       issuer,
		"aud":       resource,
		"sub":       "user-1",
		"scope":     "orders:read",
		"token_use": "access",
		"tenant":    "acme",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	must(err)
	headers := jws.NewHeaders()
	must(headers.Set(jws.KeyIDKey, "key-1"))
	signed, err := jws.Sign(claims, jws.WithKey(jwa.EdDSA(), private, jws.WithProtectedHeaders(headers)))
	must(err)
	return jwks, string(signed)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func ExampleNewJWTVerifier() {
	jwks, token := identityProvider()

	// Build the verifier once, outside the handler. Without WithJWKS, it
	// fetches the keys from the issuer's OpenID configuration.
	verifier, err := auth.NewJWTVerifier(issuer, []string{resource}, []auth.Algorithm{auth.EdDSA},
		auth.WithJWKS(jwks),
		auth.WithRequiredClaims("sub"),
		auth.WithExpectedClaims(map[string]string{"token_use": "access"}),
	)
	if err != nil {
		panic(err)
	}

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		panic(err)
	}
	fmt.Println(claims.Subject(), claims.Scopes())

	_, err = verifier.Verify(context.Background(), "not a token")
	fmt.Println(err)
	// Output:
	// user-1 [orders:read]
	// invalid access token
}

func ExampleClaims_As() {
	jwks, token := identityProvider()
	verifier, err := auth.NewJWTVerifier(issuer, []string{resource}, []auth.Algorithm{auth.EdDSA}, auth.WithJWKS(jwks))
	if err != nil {
		panic(err)
	}
	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		panic(err)
	}

	type caller struct {
		Subject string `json:"sub"`
		Tenant  string `json:"tenant"`
	}
	who, err := claims.As[caller]()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%+v\n", who)
	// Output: {Subject:user-1 Tenant:acme}
}

func ExampleRequire() {
	jwks, token := identityProvider()
	verifier, err := auth.NewJWTVerifier(issuer, []string{resource}, []auth.Algorithm{auth.EdDSA}, auth.WithJWKS(jwks))
	if err != nil {
		panic(err)
	}

	protect, err := auth.Require(verifier, auth.WithScopes("orders:read"))
	if err != nil {
		panic(err)
	}
	orders := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := auth.ClaimsFromContext(r.Context())
		if _, err := fmt.Fprintln(w, "orders of", claims.Subject()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))

	for _, authorization := range []string{"Bearer " + token, ""} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/orders", http.NoBody)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		orders.ServeHTTP(w, r)
		fmt.Print(w.Code, " ", w.Body.String())
	}
	// Output:
	// 200 orders of user-1
	// 401 {"message":"Unauthorized"}
}

func ExampleAuthorizeToken() {
	jwks, token := identityProvider()
	verifier, err := auth.NewJWTVerifier(issuer, []string{resource}, []auth.Algorithm{auth.EdDSA}, auth.WithJWKS(jwks))
	if err != nil {
		panic(err)
	}

	// The handler of a REST API TOKEN authorizer, for lambda.Start.
	authorizer := func(ctx context.Context, event events.APIGatewayCustomAuthorizerRequest) (events.APIGatewayCustomAuthorizerResponse, error) {
		return auth.AuthorizeToken(ctx, verifier, event, auth.WithScopes("orders:read"), auth.WithContextClaims("tenant"))
	}

	out, err := authorizer(context.Background(), events.APIGatewayCustomAuthorizerRequest{
		Type:               "TOKEN",
		AuthorizationToken: "Bearer " + token,
		MethodArn:          "arn:aws:execute-api:eu-west-1:123456789012:abc123/prod/GET/orders",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(out.PrincipalID, out.PolicyDocument.Statement[0].Effect, out.Context)
	// Output: user-1 Allow map[tenant:acme]
}

func ExampleNewCognitoVerifier() {
	verifier, err := auth.NewCognitoVerifier("eu-west-1_AbC123", "orders-client", []string{resource})
	if err != nil {
		panic(err)
	}
	fmt.Println(verifier.Issuer())
	// Output: https://cognito-idp.eu-west-1.amazonaws.com/eu-west-1_AbC123
}

func ExampleAnyOf() {
	corporate, err := auth.NewJWTVerifier("https://login.example.com/", []string{resource}, []auth.Algorithm{auth.RS256})
	if err != nil {
		panic(err)
	}
	cognito, err := auth.NewCognitoVerifier("eu-west-1_AbC123", "orders-client", []string{resource})
	if err != nil {
		panic(err)
	}
	verifier, err := auth.AnyOf(corporate, cognito)
	if err != nil {
		panic(err)
	}

	// A token from an issuer nobody configured is refused without a request.
	_, err = verifier.Verify(context.Background(), "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2V2aWwuZXhhbXBsZS5jb20vIn0.c2ln")
	fmt.Println(err)
	// Output: invalid access token
}
