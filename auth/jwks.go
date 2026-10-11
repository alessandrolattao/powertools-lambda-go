package auth

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/alessandrolattao/powertools-lambda-go/internal/authval"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// maxDocumentBytes bounds the discovery and JWKS documents read, as
// Powertools does.
const maxDocumentBytes = 1 << 20

// The failed refresh backoff: 1, 2, 4, 8, 16, then 30 seconds.
const (
	maxBackoff  = 30 * time.Second
	maxFailures = 5
)

// Why a refresh failed. They only reach the debug log: callers see
// ErrJWKSFetch, never the identity provider's answer.
var (
	errStatus          = errors.New("unexpected HTTP status")
	errTooLarge        = errors.New("document larger than 1 MiB")
	errNotObject       = errors.New("document is not a JSON object")
	errIssuerMismatch  = errors.New("discovery document names another issuer")
	errInvalidJWKSURI  = errors.New("discovery document has no valid HTTPS jwks_uri")
	errBudgetExhausted = errors.New("verification budget exhausted")
)

// noRedirect stops a client at the first response: a redirect could take
// the key request to a host nobody trusted.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// defaultClient sends the discovery and key requests of verifiers that do
// not bring their own client.
var defaultClient = &http.Client{CheckRedirect: noRedirect}

// cacheIdentity is what makes two verifiers share keys, as Powertools
// shares a cache between verifiers with the same issuer, key URL and
// timings. The HTTP client is part of it: verifiers that reach the
// provider differently do not share.
type cacheIdentity struct {
	issuer   string
	uri      string
	maxAge   time.Duration
	cooldown time.Duration
	client   *http.Client
}

// jwksCache holds the signing keys fetched for one identity. It is lazy:
// keys are fetched by the verification that needs them, never in the
// background, since Lambda freezes the execution environment between
// invocations. Concurrent verifications share one refresh.
type jwksCache struct {
	id     cacheIdentity
	client *http.Client
	now    func() time.Time

	mu sync.Mutex
	// keys is the last key set fetched, trusted until expiresAt.
	keys      *keySet
	expiresAt time.Time
	// nextUnknownRefresh rate-limits refreshes for unknown kids.
	nextUnknownRefresh time.Time
	// retryAt and failures implement the backoff after failed refreshes.
	retryAt  time.Time
	failures int
	// flight is the refresh in progress, nil when there is none.
	flight *flight
}

// flight is one refresh. done is closed when it ends; abandoned reports a
// refresh cut short by its caller going away, which says nothing about the
// identity provider.
type flight struct {
	done      chan struct{}
	abandoned bool
}

func newJWKSCache(id cacheIdentity, now func() time.Time) *jwksCache {
	client := defaultClient
	if id.client != nil {
		client = &http.Client{
			Transport:     id.client.Transport,
			Jar:           id.client.Jar,
			Timeout:       id.client.Timeout,
			CheckRedirect: noRedirect,
		}
	}
	return &jwksCache{id: id, client: client, now: now}
}

// registry shares caches between verifiers with the same identity while at
// least one of them is alive, like Powertools' weak value dictionary.
var registry = struct {
	mu     sync.Mutex
	caches map[cacheIdentity]weak.Pointer[jwksCache]
}{caches: make(map[cacheIdentity]weak.Pointer[jwksCache])}

func sharedCache(id cacheIdentity) *jwksCache {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if cache := registry.caches[id].Value(); cache != nil {
		return cache
	}
	cache := newJWKSCache(id, time.Now)
	registry.caches[id] = weak.Make(cache)
	runtime.AddCleanup(cache, forgetCache, id)
	return cache
}

// forgetCache drops the entry of a collected cache, unless a new cache took
// its place in the meantime.
func forgetCache(id cacheIdentity) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.caches[id].Value() == nil {
		delete(registry.caches, id)
	}
}

// step is what a caller of get does next.
type step int

const (
	stepServe   step = iota // the keys are fresh and hold the kid
	stepWait                // another caller is refreshing
	stepUnknown             // the kid is unknown and may not trigger a refresh
	stepBackoff             // the last refresh failed too recently
	stepRefresh             // this caller refreshes
)

// get returns the current key set, refreshing it when it has expired or,
// rate-limited, when it lacks kid. A wanted kid that stays unknown after a
// refresh is an invalid token, not a fetch failure. timeout bounds the
// refresh and the wait for another caller's one.
func (c *jwksCache) get(parent context.Context, timeout time.Duration, kid string, wantKID bool) (*keySet, error) {
	ctx, cancel := context.WithTimeoutCause(parent, timeout, errBudgetExhausted)
	defer cancel()
	joined := false
	for {
		c.mu.Lock()
		next, keys, f := c.next(kid, wantKID, joined)
		switch next {
		case stepServe:
			c.mu.Unlock()
			return keys, nil
		case stepWait:
			c.mu.Unlock()
			select {
			case <-f.done:
				joined = joined || !f.abandoned
			case <-ctx.Done():
				return nil, errJWKSFetch()
			}
		case stepUnknown:
			c.mu.Unlock()
			return nil, errInvalidToken()
		case stepBackoff:
			c.mu.Unlock()
			return nil, errJWKSFetch()
		case stepRefresh:
			if ctx.Err() != nil {
				// The budget ran out while waiting: no refresh is started,
				// so none can fail.
				c.mu.Unlock()
				return nil, errJWKSFetch()
			}
			previous := c.nextUnknownRefresh
			f = &flight{done: make(chan struct{})}
			c.flight = f
			c.nextUnknownRefresh = c.now().Add(c.id.cooldown)
			c.mu.Unlock()
			return c.refresh(parent, ctx, f, previous)
		}
	}
}

// next decides the step of get. It runs with c.mu held.
func (c *jwksCache) next(kid string, wantKID, joined bool) (step, *keySet, *flight) {
	now := c.now()
	fresh := c.keys != nil && now.Before(c.expiresAt)
	switch {
	case fresh && (!wantKID || c.keys.hasKID(kid)):
		return stepServe, c.keys, nil
	case c.flight != nil:
		return stepWait, nil, c.flight
	case fresh && (joined || now.Before(c.nextUnknownRefresh)):
		return stepUnknown, nil, nil
	case now.Before(c.retryAt):
		return stepBackoff, nil, nil
	default:
		return stepRefresh, nil, nil
	}
}

// refresh fetches the key set and replaces the cached one whole, so that
// keys the provider removed disappear. A failure backs off and leaves the
// old keys to expire: they are never served past their maximum age.
func (c *jwksCache) refresh(parent, ctx context.Context, f *flight, previousUnknown time.Time) (*keySet, error) {
	keys, err := c.fetch(ctx)
	if err == nil && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	c.mu.Lock()
	defer func() {
		c.flight = nil
		close(f.done)
		c.mu.Unlock()
	}()
	switch {
	case err == nil:
		c.keys = keys
		c.expiresAt = c.now().Add(c.id.maxAge)
		c.retryAt = time.Time{}
		c.failures = 0
		return keys, nil
	case parent.Err() != nil:
		// The caller went away before the provider answered: let the next
		// caller try at once, without blaming the provider.
		f.abandoned = true
		c.nextUnknownRefresh = previousUnknown
		return nil, errJWKSFetch()
	default:
		c.retryAt = c.now().Add(min(time.Second<<c.failures, maxBackoff))
		c.failures = min(c.failures+1, maxFailures)
		debug.Logger().DebugContext(ctx, "auth: refreshing the signing keys failed", "err", err, "failures", c.failures)
		return nil, errJWKSFetch()
	}
}

// fetch downloads the key set: from the configured JWKS URI, or from the
// one the issuer's OpenID configuration names, which must name the issuer
// exactly.
func (c *jwksCache) fetch(ctx context.Context) (*keySet, error) {
	uri := c.id.uri
	if uri == "" {
		discovery := strings.TrimRight(c.id.issuer, "/") + "/.well-known/openid-configuration"
		doc, err := c.getJSON(ctx, discovery)
		if err != nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		if issuer, ok := jsonString(doc["issuer"]); !ok || issuer != c.id.issuer {
			return nil, errIssuerMismatch
		}
		if uri, _ = jsonString(doc["jwks_uri"]); !authval.HTTPSURL(uri, false) {
			return nil, errInvalidJWKSURI
		}
	}
	doc, err := c.getJSON(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	return parseKeySet(doc)
}

// getJSON reads a JSON object of at most 1 MiB from target, which must
// answer 200.
func (c *jwksCache) getJSON(ctx context.Context, target string) (map[string]jsontext.Value, error) {
	//nolint:gosec // G704: target is the configured or discovered key endpoint, validated as HTTPS.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req) //nolint:gosec // G704: see above.
	if err != nil {
		return nil, fmt.Errorf("send the request: %w", err)
	}
	defer closeBody(ctx, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w %d", errStatus, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the response: %w", err)
	}
	if len(body) > maxDocumentBytes {
		return nil, errTooLarge
	}
	var doc map[string]jsontext.Value
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return nil, errNotObject
	}
	return doc, nil
}

func closeBody(ctx context.Context, body io.Closer) {
	if err := body.Close(); err != nil {
		debug.Logger().DebugContext(ctx, "auth: closing a response body failed", "err", err)
	}
}
