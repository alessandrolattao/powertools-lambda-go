# Auth (alpha)

JWT access-token verification for Lambda functions, the Go side of
Powertools Python's `auth_alpha` JWT utility.

> **Alpha.** Upstream ships this utility under `auth_alpha` while it collects
> feedback. The Go API may change while upstream's does.

Verify a token directly with `Verify`, protect `net/http` routes with
`Require`, or answer an API Gateway Lambda authorizer with the `Authorize`
functions. For tokens your function sends to other APIs, see
[OAuth2 client](oauth2.md).

## Key features

* Verify JWT signatures, issuer, audience, expiration and required claims,
  with an explicit allow-list of asymmetric algorithms (RS, PS, ES, ES256K,
  EdDSA).
* Reuse signing keys across warm invocations and refresh them on rotation;
  never use keys whose refresh failed past their maximum age.
* Protect Event Handler (`net/http`) routes with scopes and custom
  authorization.
* Build REST API and HTTP API Lambda authorizer responses.
* Credential-free errors with a stable reason and a retryable flag.

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go/auth
```

The package depends on `github.com/lestrrat-go/jwx/v4` for JWK parsing and
signature checks, and on `github.com/jwx-go/es256k/v4` for ES256K.

### Create a verifier

Create the verifier once, outside the handler, so that warm invocations
reuse its signing keys. Configure the trusted issuer, the audience your
function serves, and the algorithms the issuer signs with:

```go
verifier, err := auth.NewJWTVerifier(
	os.Getenv("ISSUER_URL"),
	[]string{os.Getenv("RESOURCE_URL")},
	[]auth.Algorithm{auth.RS256},
	auth.WithRequiredClaims("sub"),
	// Adapt this constraint to your provider's access-token profile.
	auth.WithExpectedClaims(map[string]string{"token_use": "access"}),
)
if err != nil {
	log.Fatal(err)
}
```

The issuer must be an HTTPS URL, and the token's `iss` claim must equal it
byte for byte. Without `WithJWKS` or `WithJWKSURI`, the verifier reads the
keys' location from `<issuer>/.well-known/openid-configuration`, which must
announce the same issuer.

| Function | Use when |
| --- | --- |
| `verifier.Verify(ctx, token)` | You already have the encoded JWT |
| `auth.VerifyAuthorizationHeader(ctx, verifier, value)` | You have a whole `Authorization` header value |
| `auth.VerifyRequest(verifier, r)` | You have an `*http.Request` |
| `auth.Require(verifier, ...)` | You want to protect `net/http` handlers |
| `auth.AuthorizeToken`, `AuthorizeRequest`, `AuthorizeHTTP`, `AuthorizeHTTPSimple`, `AuthorizeHTTPV1` | The function is an API Gateway authorizer |
| `verifier.Prefetch(ctx)` | You want the keys during INIT |

### Verify a JWT

`Verify` takes a JWT without the `Bearer` prefix and returns its claims, or
an error. Invalid tokens match `auth.ErrInvalidToken`; when the discovery or
JWKS endpoint is unavailable, the error matches `auth.ErrJWKSFetch` instead.
Your function decides how each maps to its event source.

```go
claims, err := verifier.Verify(ctx, token)
if err != nil {
	return err
}
fmt.Println(claims.Subject(), claims.Scopes())
```

`Claims` has accessors for the registered claims (`Issuer`, `Subject`,
`Audience`, `ExpiresAt`, `Scopes`...), `Get` and `GetString` for the others,
and `As` decodes them all into a struct of yours:

```go
type caller struct {
	Subject string `json:"sub"`
	Tenant  string `json:"tenant"`
}
who, err := claims.As[caller]()
```

### Required resources

JWT verification needs no IAM permission. Discovery and remote key sets
need outbound HTTPS: private subnets may need NAT or private connectivity.
A static key set (`WithJWKS`) needs no network, but rotating it is up to
you.

### Protect an HTTP route

`Require` returns a `net/http` middleware bound to the verifier and the
required scopes. The Event Handler adapts Lambda events to `net/http`, so
the middleware plugs into its routes as it does into a `ServeMux`:

```go
protect, err := auth.Require(verifier, auth.WithScopes("orders:read"))
if err != nil {
	log.Fatal(err)
}
mux := http.NewServeMux()
mux.Handle("GET /orders", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	fmt.Fprintln(w, "orders of", claims.Subject())
})))
```

For each request, the middleware reads the Bearer token of the
`Authorization` header, verifies it, checks that it grants `orders:read`,
and hands the claims to the handler through the request's context. The
handler does not run when any of this fails:

| Failure | Response |
| --- | --- |
| Missing or invalid token | 401, `WWW-Authenticate: Bearer` (with `error="invalid_token"` for an invalid one) |
| Missing scope | 403, `WWW-Authenticate: Bearer error="insufficient_scope", scope="..."` |
| Custom authorization refused | 403 |
| Discovery or JWKS endpoint unavailable | 503 |

The body is `{"message":"Unauthorized"}` (or `Forbidden`, or
`Service Unavailable`). Configure public routes and CORS preflight
separately.

### Verify an Authorization header

Without the middleware, pass the whole header value:

```go
claims, err := auth.VerifyAuthorizationHeader(ctx, verifier, event.Headers["authorization"])
```

The scheme must be `Bearer`, in any case. An empty value is a missing
header (`auth.ErrMissingToken`). Both verification paths require `iss`,
`aud` and `exp`; `WithRequiredClaims` adds more.

## Advanced

### Customize route authorization

`WithAuthorize` adds your own policy, run after verification and the scope
check; `WithErrorResponse` writes the response of a refused request, for
instance to log its reason:

```go
protect, err := auth.Require(verifier,
	auth.WithScopes("orders:read"),
	auth.WithAuthorize(func(r *http.Request, claims *auth.Claims) bool {
		tenant, _ := claims.GetString("tenant")
		return tenant == r.PathValue("tenant")
	}),
	auth.WithErrorResponse(func(w http.ResponseWriter, r *http.Request, f auth.Failure) {
		logger.WarnContext(r.Context(), "authorization failed", "reason", f.Reason, "retryable", f.Retryable)
		maps.Copy(w.Header(), f.Header)
		w.WriteHeader(f.StatusCode)
	}),
)
```

`Failure` carries no token data. Keep its status and headers to keep the
HTTP contract.

### Key freshness and Lambda timeouts

| Option | Default | Purpose |
| --- | --- | --- |
| `WithTimeout` | 3 seconds | Bounds the discovery and JWKS requests, and the wait for another caller's refresh |
| `WithJWKSMaxAge` | 5 minutes | How long fetched keys are trusted |
| `WithUnknownKIDCooldown` | 5 minutes | Minimum interval between refreshes caused by unknown key IDs |
| `WithClockSkew` | 1 minute | Tolerance on `exp`, `nbf` and `iat` |

The first verification fetches the keys unless they are static; warm
invocations reuse them. A refresh replaces the whole key set, dropping
removed keys. Keys past their maximum age whose refresh fails are never
used: the verification fails with `auth.ErrJWKSFetch`, and further
refreshes back off for 1, 2, 4, 8, 16, then 30 seconds. Concurrent
verifications, as Lambda Managed Instances run them, share one refresh.

> **Leave time for Lambda to handle the error.** Keep `WithTimeout` below the
> function's timeout, so that the function answers before Lambda stops it.

`Prefetch` fetches the keys during INIT:

```go
if err := verifier.Prefetch(ctx); err != nil {
	log.Fatal(err)
}
```

Prefetching saves the first invocation a round trip, but an identity
provider outage then fails the cold start.

### Cognito

`NewCognitoVerifier` is the profile of resource-bound Amazon Cognito access
tokens:

```go
verifier, err := auth.NewCognitoVerifier(
	os.Getenv("COGNITO_USER_POOL_ID"),
	os.Getenv("COGNITO_CLIENT_ID"),
	[]string{os.Getenv("RESOURCE_URL")},
)
```

It checks RS256, `token_use` equal to `access`, the app client in
`client_id`, and the resource audience. Cognito ID tokens, and access tokens
issued without the resource audience, are refused.

`AnyOf` combines verifiers when a function trusts several issuers. A token
goes to the verifier of its `iss` claim; tokens of any other issuer are
refused without a network request.

```go
verifier, err := auth.AnyOf(corporate, cognito)
```

### Lambda authorizers

The `Authorize` functions answer API Gateway authorizers, one per event
type of `aws-lambda-go`:

```go
func handler(ctx context.Context, event events.APIGatewayCustomAuthorizerRequest) (events.APIGatewayCustomAuthorizerResponse, error) {
	return auth.AuthorizeToken(ctx, verifier, event,
		auth.WithScopes("orders:read"),
		auth.WithContextClaims("sub"),
		auth.WithOnError(func(ctx context.Context, err *auth.Error) {
			logger.WarnContext(ctx, "authorization failed", "reason", err.Reason, "retryable", err.Retryable)
		}),
	)
}
```

| Function | Event | Response |
| --- | --- | --- |
| `AuthorizeToken` | REST API TOKEN | IAM policy |
| `AuthorizeRequest` | REST API REQUEST | IAM policy |
| `AuthorizeHTTPV1` | HTTP API, payload 1.0 | IAM policy |
| `AuthorizeHTTP` | HTTP API, payload 2.0 | IAM policy |
| `AuthorizeHTTPSimple` | HTTP API, payload 2.0 | simple response |

Invalid tokens and missing scopes return a Deny (or `isAuthorized: false`).
When the signing keys are unavailable, the function returns an error
matching `auth.ErrJWKSFetch`, the invocation fails and API Gateway answers
5xx. An IAM Allow requires a `sub` claim, names it as the principal, and
grants the invoked method or route ARN only, never a wildcard.
`WithContextClaims` copies selected scalar claims into the authorizer
context; none is copied by default.

If you enable API Gateway's authorizer caching, include every
authorization input in its identity sources: a cached Allow outlives the
token's expiry until the TTL ends. Keep it disabled when every request must
respect token expiration.

### Errors and diagnostics

Every failure is an `*auth.Error` with a stable `Reason` and a `Retryable`
flag. Its message is fixed and never carries the token, the claims or the
provider's answer.

| Reason | Sentinels | Retryable |
| --- | --- | --- |
| `missing_token` | `ErrMissingToken`, `ErrInvalidToken` | false |
| `invalid_token` | `ErrInvalidToken` | false |
| `invalid_claims` | `ErrInvalidClaims`, `ErrInvalidToken` | false |
| `token_expired` | `ErrTokenExpired`, `ErrInvalidToken` | false |
| `invalid_signature` | `ErrInvalidSignature`, `ErrInvalidToken` | false |
| `insufficient_scope` | `ErrInsufficientScope`, `ErrForbidden` | false |
| `forbidden` | `ErrForbidden` | false |
| `jwks_unavailable` | `ErrJWKSFetch` | true |

```go
if authErr, ok := errors.AsType[*auth.Error](err); ok {
	logger.WarnContext(ctx, "token refused", "reason", authErr.Reason, "retryable", authErr.Retryable)
}
```

Log the reason and the retryable flag, never tokens, claims or request
headers. The package itself logs nothing; set `POWERTOOLS_DEBUG` to see why
a key refresh failed.

## Testing your code

`authtest.NewVerifier` returns fixed claims for any token, without
cryptography or network, so that tests exercise the HTTP request, the
Bearer extraction and the scope check:

```go
verifier := authtest.NewVerifier(map[string]any{"sub": "test-user", "scope": "orders:read"})
protect, err := auth.Require(verifier, auth.WithScopes("orders:read"))
if err != nil {
	panic(err)
}
handler := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	claims, _ := auth.ClaimsFromContext(r.Context())
	w.Header().Set("X-Subject", claims.Subject())
}))

r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/orders", http.NoBody)
r.Header.Set("Authorization", "Bearer any-token")
w := httptest.NewRecorder()
handler.ServeHTTP(w, r)
fmt.Println(w.Code, w.Header().Get("X-Subject"))
// Output: 200 test-user
```

`auth.ContextWithClaims` puts claims in a context to call a handler
directly. Keep separate tests for the token profiles your function accepts.

## Differences from Powertools Python

See the package documentation (`go doc github.com/alessandrolattao/powertools-lambda-go/auth`)
for the complete list. In short: configuration errors are returned rather
than raised; `Require` is `net/http` middleware with the claims in the
request's context; authorizers take the `aws-lambda-go` event types; JSON
with duplicate member names is refused; EdDSA covers Ed25519 only; and a
refresh abandoned by its caller does not count as a provider failure.
