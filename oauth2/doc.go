// Package oauth2 acquires and caches OAuth 2.0 client-credentials access
// tokens for a Lambda function that calls an API on its own behalf:
// Powertools for AWS Lambda (Python)'s OAuth2 client (auth_alpha.oauth2).
// Upstream marks the utility alpha, so this API may change while it is.
//
// To authenticate incoming requests, see the auth package. This client
// obtains separate credentials for outgoing ones; it never forwards a
// caller's token.
//
// # Getting a token
//
// A Client serves one provider and one resource. Build it once, outside
// the handler, so that warm invocations reuse its token:
//
//	client, err := oauth2.New(os.Getenv("TOKEN_URL"), os.Getenv("CLIENT_ID"), secret,
//		oauth2.WithScopes(strings.Fields(os.Getenv("SCOPES"))...),
//		oauth2.WithResource("https://inventory.example.com"),
//		oauth2.WithTimeout(5*time.Second),
//	)
//
// AuthHeaders returns the Authorization header for an HTTP client of your
// own, Token the bare token. HTTPClient and Transport send requests with
// the token injected, which is Powertools' request(): HTTPS only, no
// redirects, no retries, a request timeout of its own (WithRequestTimeout,
// five seconds), and no Authorization header of the caller's.
//
// # The secret
//
// The secret comes from a SecretSource, called for every token request,
// retries included, and never cached by the client: a rotated secret is
// used as soon as the source returns it. Powertools recommends a Secrets
// Manager lookup through the parameters package with a five minute maximum
// age and tight SDK timeouts; StaticSecret covers a secret read once, such
// as from an environment variable. An error from the source, or an empty
// secret, fails the acquisition without retry, and its content never
// reaches the caller.
//
// # Exchange, caching and retries
//
// The token request is a client-credentials grant sent with
// golang.org/x/oauth2/clientcredentials: client_secret_basic by default,
// client_secret_post with WithAuthMethod, never switching between them; the
// scopes, and the audience (WithAudience) or the RFC 8707 resource
// (WithResource). The answer must be a 200 JSON object of at most 1 MiB
// with a b64token access_token, a bearer token_type and, when present, a
// positive expires_in.
//
//   - A token is reused until 30 seconds before it expires. Tokens without
//     a lifetime, or living 30 seconds or less, are returned uncached. The
//     lifetime counts from just before the request, so a slow secret
//     lookup does not shorten it.
//   - Concurrent callers share one exchange, its failure and its
//     uncacheable token included; each waits within its own timeout.
//   - Transport failures, 429 and 5xx answers are retried twice, after 100
//     then 200 milliseconds, while the budget (WithTimeout, three seconds)
//     allows. The budget covers waiting, secret lookups, requests and
//     backoff.
//   - Clients never share tokens.
//
// # Errors
//
// A failed acquisition is an *Error matching ErrTokenExchange, with
// Retryable set for transient failures and an exhausted budget; a failed
// authenticated request matches ErrDownstreamRequest and is never
// retryable, since the server may have acted on it. Messages are fixed and
// never carry the secret, the token or the provider's answer. Invalid
// configuration matches ErrInvalidConfig, a refused request
// ErrInvalidRequest. The package logs nothing, except to the
// POWERTOOLS_DEBUG log.
//
// # Not supported
//
// As in Powertools: other grants, refresh tokens, introspection and
// revocation, private_key_jwt, mTLS and DPoP client authentication,
// discovery, and routing or failover between clients.
//
// # Differences from Powertools Python
//
//   - client_secret, a string or a callable, is a SecretSource; an empty
//     StaticSecret fails at the first acquisition rather than at
//     construction.
//   - request() is HTTPClient and Transport: responses are streamed, not
//     buffered, and the request timeout lasts until the body is closed.
//     The method, a string of letters, is upper-cased; the header checks
//     apply byte by byte.
//   - x/oauth2 parses the token response, which makes four answers fail
//     that Python accepts or refuses differently: an expires_in with a
//     fraction or an exponent is refused; an expires_in of null counts as
//     absent; a 200 answer carrying an error member is refused; and an
//     answer labelled application/x-www-form-urlencoded or text/plain is
//     refused even when its body is JSON.
//   - An exchange cut short because its owner's context ended, rather than
//     by the budget, is not shared with the callers waiting on it: they
//     exchange again. Python has no such case.
package oauth2
