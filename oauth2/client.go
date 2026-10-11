package oauth2

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	xoauth2 "golang.org/x/oauth2"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
)

// The defaults of Powertools Python's OAuth2Client.
const (
	// DefaultTimeout bounds one token acquisition: waiting for another
	// caller's exchange, the secret lookup, the token requests and the
	// backoff between them.
	DefaultTimeout = 3 * time.Second
	// DefaultRequestTimeout bounds an authenticated request after its token
	// is acquired, reading the response included.
	DefaultRequestTimeout = 5 * time.Second
)

// earlyRenewal is how long before its expiry a cached token is replaced;
// tokens that live shorter than this are never cached.
const earlyRenewal = 30 * time.Second

// SecretSource returns the client secret. The client calls it for every
// token request, retries included, and never keeps what it returns, so that
// a rotated secret is picked up as soon as the source sees it. Wrap a
// Secrets Manager or Parameter Store lookup in one, with its own cache
// (Powertools recommends a five minute maximum age) and tight SDK timeouts.
type SecretSource func(ctx context.Context) (string, error)

// StaticSecret is a SecretSource that always returns secret, such as one
// read from an environment variable at startup. It never rotates.
func StaticSecret(secret string) SecretSource {
	return func(context.Context) (string, error) { return secret, nil }
}

// AuthMethod is how the client authenticates to the token endpoint.
type AuthMethod string

// The client authentication methods. The client never switches from one to
// the other on its own.
const (
	// ClientSecretBasic sends the form-encoded client ID and secret with
	// HTTP Basic authentication, and neither in the body.
	ClientSecretBasic AuthMethod = "client_secret_basic"
	// ClientSecretPost sends client_id and client_secret in the form body,
	// without an Authorization header.
	ClientSecretPost AuthMethod = "client_secret_post"
)

// Option configures a Client.
type Option func(*config)

type config struct {
	method         AuthMethod
	scopes         []string
	audience       string
	audienceSet    bool
	resource       string
	resourceSet    bool
	timeout        time.Duration
	requestTimeout time.Duration
	client         *http.Client
	now            func() time.Time
}

// WithAuthMethod chooses how the client authenticates; the default is
// ClientSecretBasic.
func WithAuthMethod(method AuthMethod) Option {
	return func(c *config) { c.method = method }
}

// WithScopes requests scopes, which must be OAuth scope tokens, on every
// exchange.
func WithScopes(scopes ...string) Option {
	return func(c *config) { c.scopes = append(c.scopes, scopes...) }
}

// WithAudience sends the audience request parameter some providers, such
// as Auth0, use to select the API. It excludes WithResource.
func WithAudience(audience string) Option {
	return func(c *config) { c.audience, c.audienceSet = audience, true }
}

// WithResource sends an RFC 8707 resource indicator: an absolute URI
// without a fragment, URNs included. It excludes WithAudience.
func WithResource(resource string) Option {
	return func(c *config) { c.resource, c.resourceSet = resource, true }
}

// WithTimeout bounds one token acquisition; the default is three seconds.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithRequestTimeout bounds an authenticated request sent through
// Transport or HTTPClient once its token is acquired, until its response
// body is closed; the default is five seconds.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *config) { c.requestTimeout = d }
}

// WithHTTPClient sends the token requests, and by default the
// authenticated ones, through client's transport, for a proxy,
// instrumentation or a private certificate authority. Redirects are never
// followed, whatever the client says.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) { c.client = client }
}

// withClock replaces the clock, for tests.
func withClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// Client acquires OAuth 2.0 client-credentials access tokens for one
// provider and one resource, and caches them. Build it once, outside the
// handler, so that warm invocations reuse its token; it is safe for
// concurrent use, and concurrent callers share one exchange.
type Client struct {
	tokenURL       string
	clientID       string
	secret         SecretSource
	style          xoauth2.AuthStyle
	scopes         []string
	params         url.Values
	timeout        time.Duration
	requestTimeout time.Duration
	transport      http.RoundTripper
	now            func() time.Time

	mu     sync.Mutex
	cached *accessToken
	flight *exchange
}

// New returns a client that acquires tokens from tokenURL, an HTTPS URL,
// as clientID with the secret source returns. It fetches nothing: tokens
// are acquired when first needed. Configuration errors match
// ErrInvalidConfig and never quote the values they refuse.
func New(tokenURL, clientID string, secret SecretSource, opts ...Option) (*Client, error) {
	cfg := config{method: ClientSecretBasic, timeout: DefaultTimeout, requestTimeout: DefaultRequestTimeout, now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	if !authval.HTTPSURL(tokenURL, false) {
		return nil, fmt.Errorf("%w: the token URL must be an HTTPS URL without user information or a fragment", ErrInvalidConfig)
	}
	if !authval.NonBlank(clientID) {
		return nil, fmt.Errorf("%w: a nonblank client ID is required", ErrInvalidConfig)
	}
	if secret == nil {
		return nil, fmt.Errorf("%w: a client secret source is required", ErrInvalidConfig)
	}
	c := &Client{
		tokenURL: tokenURL, clientID: clientID, secret: secret,
		timeout: cfg.timeout, requestTimeout: cfg.requestTimeout, now: cfg.now,
		transport: http.DefaultTransport,
	}
	if cfg.client != nil && cfg.client.Transport != nil {
		c.transport = cfg.client.Transport
	}
	if err := c.configure(cfg); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) configure(cfg config) error {
	switch cfg.method {
	case ClientSecretBasic:
		c.style = xoauth2.AuthStyleInHeader
	case ClientSecretPost:
		c.style = xoauth2.AuthStyleInParams
	default:
		return fmt.Errorf("%w: the auth method must be client_secret_basic or client_secret_post", ErrInvalidConfig)
	}
	if cfg.audienceSet && cfg.resourceSet {
		return fmt.Errorf("%w: audience and resource are mutually exclusive", ErrInvalidConfig)
	}
	var ok bool
	if c.scopes, ok = authval.Scopes(cfg.scopes); !ok {
		return fmt.Errorf("%w: scopes must be valid OAuth scope tokens", ErrInvalidConfig)
	}
	if cfg.timeout <= 0 || cfg.requestTimeout <= 0 {
		return fmt.Errorf("%w: timeouts must be positive", ErrInvalidConfig)
	}
	c.params = url.Values{}
	if cfg.audienceSet {
		if !authval.NonBlank(cfg.audience) {
			return fmt.Errorf("%w: the audience must not be blank", ErrInvalidConfig)
		}
		c.params.Set("audience", cfg.audience)
	}
	if cfg.resourceSet {
		if !authval.NonBlank(cfg.resource) || !validResource(cfg.resource) {
			return fmt.Errorf("%w: the resource must be an absolute URI without a fragment", ErrInvalidConfig)
		}
		c.params.Set("resource", cfg.resource)
	}
	return nil
}

// validResource reports whether r is an absolute URI as Powertools checks
// it: a scheme starting with a letter, then only URI characters and
// complete percent escapes, and no fragment.
func validResource(r string) bool {
	scheme, _, found := strings.Cut(r, ":")
	if !found || !validScheme(scheme) {
		return false
	}
	for i := 0; i < len(r); i++ {
		switch ch := r[i]; {
		case ch == '%':
			if i+2 >= len(r) || !isHex(r[i+1]) || !isHex(r[i+2]) {
				return false
			}
			i += 2
		case !isLetter(ch) && !isDigit(ch) && !strings.ContainsRune("-._~:/?[]@!$&'()*+,;=", rune(ch)):
			return false
		}
	}
	return utf8.ValidString(r)
}

// validScheme reports a URI scheme: a letter, then letters, digits, "+",
// "-" or ".".
func validScheme(scheme string) bool {
	if scheme == "" || !isLetter(scheme[0]) {
		return false
	}
	for i := 1; i < len(scheme); i++ {
		if ch := scheme[i]; !isLetter(ch) && !isDigit(ch) && !strings.ContainsRune("+-.", rune(ch)) {
			return false
		}
	}
	return true
}

func isLetter(ch byte) bool { return ch|0x20 >= 'a' && ch|0x20 <= 'z' }
func isDigit(ch byte) bool  { return ch >= '0' && ch <= '9' }
func isHex(ch byte) bool    { return isDigit(ch) || (ch|0x20 >= 'a' && ch|0x20 <= 'f') }

// String hides the configuration, as Powertools' repr does.
func (*Client) String() string { return "<OAuth2Client>" }

// AuthHeaders returns the Authorization header that carries an access
// token, acquired or taken from the cache: Powertools' auth_headers(). Never
// log it, nor send it anywhere but to the resource it is for. A failure
// matches ErrTokenExchange.
func (c *Client) AuthHeaders(ctx context.Context) (http.Header, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	return http.Header{"Authorization": {"Bearer " + token}}, nil
}

// Token returns an access token, acquired or taken from the cache. Tokens
// are reused until 30 seconds before they expire; tokens without a
// lifetime, or living 30 seconds or less, are returned but not cached.
// Concurrent callers share one exchange, its failure included, each within
// its own timeout. A failure matches ErrTokenExchange.
func (c *Client) Token(ctx context.Context) (string, error) {
	budget, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	for {
		token, f, owner := c.join()
		switch {
		case f == nil:
			return token, nil
		case owner:
			c.run(ctx, budget, f)
			if f.err != nil {
				return "", f.err
			}
			return c.result(budget, f)
		}
		retry, err := c.wait(budget, f)
		if err != nil {
			return "", err
		}
		if !retry {
			return c.result(budget, f)
		}
	}
}

// join returns the cached token when it is still worth using; otherwise
// the exchange in progress, or a new one that the caller owns.
func (c *Client) join() (token string, f *exchange, owner bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.cached.cacheable(c.now()) {
		return c.cached.value, nil, false
	}
	c.cached = nil
	if c.flight != nil {
		return "", c.flight, false
	}
	c.flight = &exchange{done: make(chan struct{})}
	return "", c.flight, true
}

// wait waits for another caller's exchange within the budget. retry
// reports an exchange its owner abandoned before the provider answered,
// which says nothing of the provider: the caller tries again, possibly as
// the new owner.
func (c *Client) wait(budget context.Context, f *exchange) (retry bool, err error) {
	select {
	case <-f.done:
		return f.abandoned, nil
	case <-budget.Done():
		return false, errTokenExchange(true)
	}
}

// result is what an exchange gives one of its callers.
func (c *Client) result(budget context.Context, f *exchange) (string, error) {
	switch {
	case budget.Err() != nil:
		return "", errTokenExchange(true)
	case f.err != nil:
		return "", errTokenExchange(f.err.Retryable)
	case !f.token.usable(c.now()):
		// A token too short-lived to be cached expired while waiting.
		return "", errTokenExchange(false)
	default:
		return f.token.value, nil
	}
}

// exchange is one acquisition, shared by the callers that arrive while it
// runs. done is closed when it ends.
type exchange struct {
	done      chan struct{}
	token     *accessToken
	err       *Error
	abandoned bool
}

// run performs the exchange as its owner and publishes the outcome.
func (c *Client) run(parent, budget context.Context, f *exchange) {
	token, err := c.acquire(budget)
	c.mu.Lock()
	defer func() {
		c.flight = nil
		close(f.done)
		c.mu.Unlock()
	}()
	if err != nil {
		f.err = err
		f.abandoned = parent.Err() != nil
		return
	}
	if token.cacheable(c.now()) {
		c.cached = token
	}
	f.token = token
}

// accessToken is a token and the time it expires, if the provider said.
type accessToken struct {
	value     string
	expiresAt time.Time
}

func (t *accessToken) cacheable(now time.Time) bool {
	return !t.expiresAt.IsZero() && now.Before(t.expiresAt.Add(-earlyRenewal))
}

func (t *accessToken) usable(now time.Time) bool {
	return t.expiresAt.IsZero() || now.Before(t.expiresAt)
}
