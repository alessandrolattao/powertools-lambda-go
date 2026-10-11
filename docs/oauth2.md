# OAuth2 client (alpha)

Acquire and cache OAuth 2.0 client-credentials tokens for downstream APIs,
the Go side of Powertools Python's `OAuth2Client`.

> **Alpha.** Upstream ships this utility under `auth_alpha` while it collects
> feedback. The Go API may change while upstream's does.

`oauth2.Client` obtains bearer tokens for a function calling an API on its
own behalf, with the client-credentials grant and `client_secret_basic`
(default) or `client_secret_post` authentication. To authenticate incoming
requests, use [Auth](auth.md): this client obtains separate credentials, it
never forwards a caller's token.

## Key features

* Cache access tokens across warm invocations and acquire new ones before
  they expire.
* Share one exchange between concurrent callers of a client.
* Look the client secret up for every exchange attempt, so that rotation is
  picked up.
* Authenticate with HTTP Basic or form-body credentials, as your provider
  requires.
* Select the API with a provider-specific audience or an RFC 8707 resource
  indicator.
* Get the `Authorization` header for your own HTTP client, or an
  `http.Client` that injects it.
* Report fixed failure reasons without exposing credentials or provider
  answers.

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go/oauth2
```

The token request goes through `golang.org/x/oauth2/clientcredentials`; the
caching, retries and checks are this package's.

### Configure your identity provider

Register a client-credentials application with access to your API, and
note its client ID, secret, HTTPS token endpoint, and the scopes or API
identifier it needs. The client neither registers applications nor
discovers endpoints.

### Call a downstream API

Create the client outside the handler, so that warm invocations reuse its
token. The secret comes from a `SecretSource`, a function the client calls
for every exchange attempt; here it reads Secrets Manager:

```go
secrets := secretsmanager.NewFromConfig(cfg)
secret := func(ctx context.Context) (string, error) {
	out, err := secrets.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(os.Getenv("CLIENT_SECRET_NAME")),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.SecretString), nil
}

opts := []oauth2.Option{
	oauth2.WithScopes(strings.Fields(os.Getenv("SCOPES"))...),
	oauth2.WithTimeout(5 * time.Second),
}
if audience := os.Getenv("AUDIENCE"); audience != "" {
	opts = append(opts, oauth2.WithAudience(audience))
}
inventory, err := oauth2.New(os.Getenv("TOKEN_URL"), os.Getenv("CLIENT_ID"), secret, opts...)
if err != nil {
	log.Fatal(err)
}
api := inventory.HTTPClient()
```

Powertools recommends reading the secret through the Parameters utility,
with a five minute maximum age and tight SDK timeouts (one second to
connect, two to read, one attempt): wrap that lookup in the
`SecretSource` the same way. `oauth2.StaticSecret` covers a secret read
once, for instance from an environment variable; it never rotates.

Then, in the handler:

```go
req, err := http.NewRequestWithContext(ctx, http.MethodGet, inventoryURL+"/stock/"+url.PathEscape(sku), http.NoBody)
if err != nil {
	return nil, err
}
resp, err := api.Do(req)
if err != nil {
	return nil, err
}
defer resp.Body.Close()
if resp.StatusCode != http.StatusOK {
	return nil, errors.New("inventory lookup failed")
}
```

The example allows five seconds for the acquisition and, by default, five
for the API request: set the function's timeout above their sum.

### Required resources

Reading the secret needs `secretsmanager:GetSecretValue`, and `kms:Decrypt`
for a customer-managed key. The token exchange needs no IAM permission.
Allow outbound HTTPS to the token endpoint and the API; private subnets may
need NAT or private connectivity.

### Choose the client authentication method

| `WithAuthMethod` | Credentials sent to the token endpoint |
| --- | --- |
| `oauth2.ClientSecretBasic` (default) | Form-encoded client ID and secret in HTTP Basic authentication; neither in the form body |
| `oauth2.ClientSecretPost` | `client_id` and `client_secret` in the form body, without an Authorization header |

Choose your provider's method: the client never falls back to the other.

### Choose how to send the API request

| Method | Use when |
| --- | --- |
| `client.AuthHeaders(ctx)` | You want the `Authorization` header for an HTTP client of your own |
| `client.Token(ctx)` | You want the bare token |
| `client.HTTPClient()` | You want an `*http.Client` that sends authenticated requests |
| `client.Transport(base)` | You want to put the token injection on a transport of your own |

All of them acquire tokens on demand: `New` fetches nothing.

### Choose the resource

| Option | Token request field | Purpose |
| --- | --- | --- |
| `WithAudience` | `audience=<value>` | Provider-specific API selection, such as an Auth0 API identifier |
| `WithResource` | `resource=<value>` | An RFC 8707 resource indicator |

Use at most one. A resource is an absolute URI without a fragment, such as
`https://inventory.example.com` or `urn:example:inventory`. Create one
client per provider and resource: the request URL does not change which
token is used.

| Provider | Token endpoint path | API selection |
| --- | --- | --- |
| Amazon Cognito | `/oauth2/token` on the user pool domain | Resource-server scopes such as `inventory/read` |
| Auth0 | `/oauth/token` | `WithAudience` set to the API identifier |
| Microsoft Entra ID | `/{tenant}/oauth2/v2.0/token` | One resource's `/.default` scope |
| Okta custom authorization server | `/oauth2/{authorizationServerId}/v1/token` | Custom API scopes |
| Keycloak | `/realms/{realm}/protocol/openid-connect/token` | Service-account roles and client scopes |

### Use your own HTTP client

```go
header, err := inventory.AuthHeaders(ctx)
if err != nil {
	return err
}
req.Header.Set("Authorization", header.Get("Authorization"))
```

Configure that client's timeouts, redirects and retries yourself, and never
log the header or send it to an untrusted destination.

## Advanced

### Execution environments and token lifetimes

Each client caches its token within one execution environment; warm
invocations reuse it, new environments acquire their own. A cached token is
reused while more than 30 seconds remain; after that the next call performs
a new client-credentials exchange (there are no refresh tokens). Tokens
without a lifetime, or with 30 seconds or less, are returned uncached.
Invalid lifetimes and tokens that expire during the acquisition are refused.
The lifetime counts from just before the token request, so a slow secret
lookup does not shorten it.

Concurrent callers of one client share an exchange, short-lived tokens and
failures included; each waits within its own budget.

### Secret rotation

The secret source runs for every exchange attempt, retries included, and
the client never keeps its result. Cached tokens remain usable after a
rotation, and a five minute cache in the source delays the new secret by as
much.

### Timeouts and cold starts

| Setting | Default | Covers |
| --- | --- | --- |
| `WithTimeout` | 3 seconds | One acquisition: waiting for another caller's exchange, secret lookups, token requests and backoff |
| `WithRequestTimeout` | 5 seconds | An authenticated request after its token, until its response body is closed |
| The secret source's own timeouts | yours | Each secret lookup |

The budgets run one after the other. Measure cold starts at your memory and
network settings: the first secret lookup and SDK setup can exceed three
seconds.

### Retries and downstream responses

Transport failures, 429 and 5xx answers of the token endpoint are retried
at most twice, after 100 then 200 milliseconds, within the acquisition
budget. Other statuses, malformed answers and secret source failures are
not retried.

Authenticated requests are never retried and their responses come back
whatever their status: even a 401 does not discard the cached token. Unlike
Python's buffered urllib3 response, the response body streams; close it.

### Destination safety

The authenticated transport requires HTTPS, follows no redirect, and
refuses, before looking the secret up, a request with an `Authorization`
header of its own, a method that is not letters only, or a header name or
value HTTP does not allow.

> **Use trusted destination URLs.** Send requests to configured URLs only,
> never to caller-controlled ones: the destination is not checked against
> the audience or the resource.

### Errors and diagnostics

| Sentinel | Reason | Retryable |
| --- | --- | --- |
| `oauth2.ErrTokenExchange` | `token_exchange_failed` | true for transient endpoint failures and an exhausted budget, false otherwise |
| `oauth2.ErrDownstreamRequest` | `downstream_request_failed` | false: the server may have performed the operation |

Invalid configuration matches `oauth2.ErrInvalidConfig` and a refused
request `oauth2.ErrInvalidRequest`, both before any token is acquired.

```go
resp, err := api.Do(req)
if err != nil {
	if e, ok := errors.AsType[*oauth2.Error](err); ok {
		logger.WarnContext(ctx, "inventory request unavailable", "reason", e.Reason, "retryable", e.Retryable)
	}
	return events.APIGatewayProxyResponse{StatusCode: http.StatusBadGateway}, nil
}
```

The client logs nothing and drops the causes of its errors; set
`POWERTOOLS_DEBUG` to see why an exchange attempt failed. Never log secrets,
tokens, request headers or bodies, or provider answers.

### Supported scope

The client treats tokens, JWTs included, as opaque strings: the downstream
API validates them. Other grants, introspection, revocation, JWT client
authentication, mTLS and DPoP are not supported.

## Testing your code

Test your handlers against an `httptest` token endpoint and API, which the
client trusts through `WithHTTPClient`:

```go
tokenURL, apiURL, trusting, stop := provider()
defer stop()
client, err := oauth2.New(tokenURL, "orders", oauth2.StaticSecret("s3cret"), oauth2.WithHTTPClient(trusting))
if err != nil {
	panic(err)
}

inventory := client.HTTPClient()
req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiURL+"/stock/item-1", http.NoBody)
if err != nil {
	panic(err)
}
resp, err := inventory.Do(req)
```

Test your provider configuration separately.

## Differences from Powertools Python

See the package documentation (`go doc github.com/alessandrolattao/powertools-lambda-go/oauth2`)
for the complete list. In short: the secret is a `SecretSource` function;
`request()` becomes an `http.Client` whose responses stream; and because
`x/oauth2` parses the token response, an `expires_in` with a fraction is
refused, a null one counts as absent, a 200 answer with an `error` member
is refused, and so is a JSON answer labelled as a form or plain text.
