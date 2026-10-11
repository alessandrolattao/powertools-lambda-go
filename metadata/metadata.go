package metadata

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// The Lambda Metadata Endpoint, as the Lambda developer guide documents it.
const (
	// APIVersion is the version of the endpoint's API the package speaks.
	APIVersion = "2026-01-15"
	// Path is where the endpoint serves the execution environment's metadata.
	Path = "/metadata/execution-environment"
	// DefaultTimeout bounds a request to the endpoint, as in Powertools
	// Python.
	DefaultTimeout = time.Second
)

// maxResponse bounds the response read from the endpoint: a few fields of
// JSON, with room for the ones AWS may add.
const maxResponse = 1 << 20

// LambdaMetadata describes the execution environment the function runs in.
type LambdaMetadata struct {
	// AvailabilityZoneID is the Availability Zone ID the environment runs in,
	// such as "use1-az1": the same physical zone in every account, unlike an
	// Availability Zone name.
	AvailabilityZoneID string `json:"AvailabilityZoneID"`
	// Raw is the whole response of the endpoint, for the fields AWS adds
	// after this package was written.
	Raw jsontext.Value `json:"-"`
}

// Sentinel errors, wrapped in a LambdaMetadataError.
var (
	// ErrNoEndpoint reports that AWS_LAMBDA_METADATA_API is not set.
	ErrNoEndpoint = errors.New("environment variable " + env.LambdaMetadataAPI + " is not set")
	// ErrNoToken reports that AWS_LAMBDA_METADATA_TOKEN is not set.
	ErrNoToken = errors.New("environment variable " + env.LambdaMetadataToken + " is not set")
	// ErrStatus reports a response other than 200 OK; StatusCode says which.
	ErrStatus = errors.New("unexpected status")
	// ErrResponse reports a response body that is not a JSON object.
	ErrResponse = errors.New("the response is not a JSON object")
)

// LambdaMetadataError reports that the metadata could not be fetched.
type LambdaMetadataError struct {
	// StatusCode is the HTTP status the endpoint answered with, or 0 when it
	// did not answer.
	StatusCode int
	// Err is the cause.
	Err error
}

func (e *LambdaMetadataError) Error() string {
	if e.StatusCode != 0 {
		return "metadata: request failed with status " + strconv.Itoa(e.StatusCode) + ": " + e.Err.Error()
	}
	return "metadata: " + e.Err.Error()
}

func (e *LambdaMetadataError) Unwrap() error { return e.Err }

// Option configures Get.
type Option func(*config)

type config struct {
	timeout time.Duration
	client  *http.Client
}

// WithTimeout bounds the request to the endpoint, DefaultTimeout by default.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithHTTPClient sends the request through client instead of a plain
// http.Client.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) { c.client = client }
}

// cache keeps the metadata for the life of the execution environment. A
// fetch in flight is shared: Lambda Managed Instances start several
// invocations at once, and they all wait for the same request.
var cache struct {
	mu       sync.Mutex
	value    *LambdaMetadata
	inflight *fetch
}

// fetch is one request to the endpoint and its outcome, ready once done is
// closed.
type fetch struct {
	done  chan struct{}
	value LambdaMetadata
	err   error
}

// Get returns the metadata of the execution environment. The first call asks
// the Lambda Metadata Endpoint; the answer is cached for the life of the
// environment, so later calls cost nothing. Call it in main to have it ready
// before the first invocation.
//
// Outside Lambda (AWS_LAMBDA_INITIALIZATION_TYPE unset, in tests and local
// runs) and when POWERTOOLS_DEV is on, Get returns empty metadata and no
// error, without a request. A failure is a *LambdaMetadataError and is not
// cached: the next call asks again.
//
// Concurrent calls share one request; each returns when it completes or when
// its own ctx ends. The options of the call that starts the request apply.
func Get(ctx context.Context, opts ...Option) (LambdaMetadata, error) {
	if env.IsDev() || env.Get(env.LambdaInitializationType) == "" {
		return LambdaMetadata{}, nil
	}
	cfg := config{timeout: DefaultTimeout, client: &http.Client{}}
	for _, opt := range opts {
		opt(&cfg)
	}

	cache.mu.Lock()
	if cache.value != nil {
		value := cache.value.clone()
		cache.mu.Unlock()
		return value, nil
	}
	f := cache.inflight
	if f == nil {
		f = &fetch{done: make(chan struct{})}
		cache.inflight = f
		// The request outlives a caller that gives up, so that the others
		// waiting for it still get the answer; the timeout bounds it.
		go f.run(context.WithoutCancel(ctx), &cfg)
	}
	cache.mu.Unlock()

	select {
	case <-f.done:
		if f.err != nil {
			return LambdaMetadata{}, f.err
		}
		return f.value.clone(), nil
	case <-ctx.Done():
		return LambdaMetadata{}, &LambdaMetadataError{Err: context.Cause(ctx)}
	}
}

// run asks the endpoint and publishes the outcome.
func (f *fetch) run(ctx context.Context, cfg *config) {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	f.value, f.err = request(ctx, cfg.client)

	cache.mu.Lock()
	// ClearCache may have dropped this fetch while it ran: its answer is not
	// the cache's to keep then.
	if cache.inflight == f {
		cache.inflight = nil
		if f.err == nil {
			cache.value = new(f.value.clone())
		}
	}
	cache.mu.Unlock()
	close(f.done)
}

// request performs the GET the Lambda developer guide documents.
func request(ctx context.Context, client *http.Client) (LambdaMetadata, error) {
	api, token := env.Get(env.LambdaMetadataAPI), env.Get(env.LambdaMetadataToken)
	switch {
	case api == "":
		return LambdaMetadata{}, &LambdaMetadataError{Err: ErrNoEndpoint}
	case token == "":
		return LambdaMetadata{}, &LambdaMetadataError{Err: ErrNoToken}
	}

	endpoint := url.URL{Scheme: "http", Host: api, Path: "/" + APIVersion + Path}
	debug.Logger().DebugContext(ctx, "fetching Lambda metadata", "url", endpoint.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return LambdaMetadata{}, &LambdaMetadataError{Err: fmt.Errorf("build the request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return LambdaMetadata{}, &LambdaMetadataError{Err: fmt.Errorf("fetch Lambda metadata: %w", err)}
	}
	defer resp.Body.Close() //nolint:errcheck // a read body has nothing left to report
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return LambdaMetadata{}, &LambdaMetadataError{StatusCode: resp.StatusCode, Err: fmt.Errorf("read the response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return LambdaMetadata{}, &LambdaMetadataError{StatusCode: resp.StatusCode, Err: ErrStatus}
	}
	value, err := decode(body)
	if err != nil {
		return LambdaMetadata{}, &LambdaMetadataError{StatusCode: resp.StatusCode, Err: err}
	}
	debug.Logger().DebugContext(ctx, "Lambda metadata", "response", string(value.Raw))
	return value, nil
}

// decode reads the endpoint's response: a JSON object whose unknown fields
// are kept in Raw and otherwise ignored, as AWS asks clients to do.
func decode(body []byte) (LambdaMetadata, error) {
	raw := jsontext.Value(bytes.TrimSpace(body))
	if !raw.IsValid() || raw.Kind() != '{' {
		return LambdaMetadata{}, ErrResponse
	}
	var value LambdaMetadata
	if err := json.Unmarshal(raw, &value); err != nil {
		return LambdaMetadata{}, fmt.Errorf("decode the response: %w", err)
	}
	value.Raw = bytes.Clone(raw)
	return value, nil
}

// clone returns a copy that shares nothing with the cache.
func (m LambdaMetadata) clone() LambdaMetadata {
	m.Raw = bytes.Clone(m.Raw)
	return m
}

// ClearCache forgets the cached metadata, so that the next Get asks the
// endpoint again. Tests use it between cases.
func ClearCache() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.value = nil
	cache.inflight = nil
}
