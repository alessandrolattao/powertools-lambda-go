// Package auth verifies JWT access tokens in Lambda functions: Powertools
// for AWS Lambda (Python)'s Auth utility (auth_alpha). Upstream marks the
// utility alpha, so this API may change while it is.
//
// # Verifying a token
//
// A JWTVerifier trusts one issuer, one or more audiences (the resource the
// function serves) and an explicit list of asymmetric algorithms. Build it
// once, outside the handler, so that warm invocations reuse its signing
// keys:
//
//	verifier, err := auth.NewJWTVerifier(
//		"https://idp.example.com/",
//		[]string{"https://orders.example.com"},
//		[]auth.Algorithm{auth.RS256},
//		auth.WithRequiredClaims("sub"),
//		auth.WithExpectedClaims(map[string]string{"token_use": "access"}),
//	)
//	...
//	claims, err := verifier.Verify(ctx, token)
//
// Verify checks, in Powertools' order: the header (an accepted alg, a kid,
// no crit, no unencoded payload), the one published key that matches it
// (kid, key type, and the alg, use and key_ops the provider published), the
// signature, the required claims (iss, aud and exp always), the issuer
// (byte for byte), the audience, exp, nbf and iat with the clock skew, then
// the expected claims and headers. The returned Claims has accessors for the
// registered claims, Get and GetString for the others, and As to decode
// them into a struct of yours.
//
// VerifyAuthorizationHeader takes a whole "Bearer <token>" header value and
// VerifyRequest an *http.Request. NewCognitoVerifier is the profile of an
// Amazon Cognito user pool, which accepts resource-bound access tokens of one
// app client only. AnyOf routes a token to the verifier of its issuer, for
// functions that trust several.
//
// # Signing keys
//
// The keys come from the issuer's OpenID configuration
// (/.well-known/openid-configuration, which must name the issuer exactly),
// from WithJWKSURI, or from a static WithJWKS set that needs no network.
// Fetched keys are cached as Powertools caches them:
//
//   - they are fetched on first use, never in the background: Lambda freezes
//     the execution environment between invocations. Prefetch fetches them
//     during initialization instead;
//   - they are trusted for WithJWKSMaxAge (five minutes), then refreshed.
//     Keys whose refresh fails are never served past that age: the
//     verification fails with ErrJWKSFetch;
//   - a token whose kid is unknown refreshes the set, at most once per
//     WithUnknownKIDCooldown (five minutes), so that key rotation works and
//     forged tokens cannot hammer the provider;
//   - a refresh replaces the whole set, so removed keys disappear;
//   - failed refreshes back off for 1, 2, 4, 8, 16, then 30 seconds;
//   - concurrent verifications share one refresh, each waiting at most
//     WithTimeout (three seconds), which also bounds the requests;
//   - verifiers with the same issuer, key URL, timings and HTTP client share
//     their keys while one of them is alive.
//
// Requests use HTTPS with certificate verification, follow no redirect,
// and read at most 1 MiB.
//
// # Event Handler middleware
//
// Require is Powertools' require(): a net/http middleware that refuses a
// request without a valid token granting the required scopes, and hands the
// verified claims to the handler through the request's context:
//
//	protect, err := auth.Require(verifier, auth.WithScopes("orders:read"))
//	...
//	mux.Handle("GET /orders", protect(listOrders))
//
//	func listOrders(w http.ResponseWriter, r *http.Request) {
//		claims, _ := auth.ClaimsFromContext(r.Context())
//		...
//	}
//
// A missing or invalid token gets 401, a missing scope or a refused
// WithAuthorize policy 403, unavailable keys 503, with RFC 6750's
// WWW-Authenticate challenge and a JSON {"message": ...} body.
// WithErrorResponse replaces that response.
//
// # Lambda authorizers
//
// AuthorizeToken and AuthorizeRequest answer REST API TOKEN and REQUEST
// authorizers, AuthorizeHTTPV1 and AuthorizeHTTP the HTTP API ones with an
// IAM policy, AuthorizeHTTPSimple with a simple response. Invalid tokens
// and missing scopes are denied; unavailable keys fail the invocation so
// that API Gateway answers 5xx. An IAM Allow names the token's sub as its
// principal and grants the invoked method or route ARN only, never a
// wildcard. WithContextClaims copies selected scalar claims into the
// authorizer context. Disable API Gateway's authorizer caching, or key it
// carefully: a cached Allow outlives the token.
//
// # Errors
//
// Every failure is an *Error with a stable Reason and a Retryable flag; its
// message is fixed and never carries the token, its claims or the provider's
// answer. The sentinels follow Powertools' hierarchy: ErrInvalidToken
// matches ErrMissingToken, ErrInvalidClaims, ErrTokenExpired and
// ErrInvalidSignature; ErrForbidden matches ErrInsufficientScope;
// ErrJWKSFetch is the retryable one. Configuration errors match
// ErrInvalidConfig, authorizer events of the wrong shape ErrInvalidEvent.
// The package logs nothing, except to the POWERTOOLS_DEBUG log.
//
// # Testing
//
// The authtest package has a Verifier that returns fixed claims without
// keys or network, the Go side of Powertools' mock_claims.
//
// # Differences from Powertools Python
//
//   - Python's ValueError at construction is an error matching
//     ErrInvalidConfig. Require returns one too, for invalid scopes.
//   - Require is net/http middleware: the claims live in the request's
//     context rather than in app.context, WithAuthorize receives the request
//     with the claims, and WithErrorResponse writes the response itself.
//   - The authorizer functions take the aws-lambda-go event types, one per
//     event kind, instead of a dict and a response_format; the IAM statement
//     lists its Action, as aws-lambda-go encodes it.
//   - VerifyAuthorizationHeader treats an empty value as a missing header,
//     since a Go string cannot be None.
//   - JSON with duplicate member names, in a token, a key set or a discovery
//     document, is refused; Python keeps the last value.
//   - A key whose curve does not fit an ES algorithm is an invalid token,
//     where PyJWT reports invalid claims. A private JWK verifies with its
//     public half.
//   - EdDSA covers Ed25519 keys; Ed448 keys, which neither the Go standard
//     library nor jwx supports natively, are unusable.
//   - A refresh cut short because its caller's context ended, rather than
//     by the timeout, is not counted as a provider failure: the next caller
//     refreshes at once instead of backing off. Python has no such case.
//   - A numeric authorizer context claim beyond int64 is copied as a
//     float64.
package auth
