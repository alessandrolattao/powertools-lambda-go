package metadata

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// The tests share the package's cache and set the environment, so none of
// them runs in parallel.

const token = "test-token"

// endpoint stands in for the Lambda Metadata Endpoint: it answers with
// status and body, and counts the requests.
type endpoint struct {
	*httptest.Server

	requests atomic.Int32
}

func newEndpoint(t *testing.T, handler http.HandlerFunc) *endpoint {
	t.Helper()
	e := &endpoint{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(e.Close)
	return e
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=43200, immutable")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) //nolint:errcheck // the test checks what the client got
	}
}

// inLambda sets what Lambda sets besides AWS_LAMBDA_INITIALIZATION_TYPE,
// with the endpoint at e.
func inLambda(t *testing.T, e *endpoint) {
	t.Helper()
	t.Setenv(env.Dev, "")
	t.Setenv(env.LambdaMetadataAPI, strings.TrimPrefix(e.URL, "http://"))
	t.Setenv(env.LambdaMetadataToken, token)
	ClearCache()
	t.Cleanup(ClearCache)
}

func TestGetAsksTheEndpoint(t *testing.T) {
	var got *http.Request
	e := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		answer(http.StatusOK, `{"AvailabilityZoneID":"use1-az1","FutureField":{"x":1}}`)(w, r)
	})
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)

	md, err := Get(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "use1-az1", md.AvailabilityZoneID)
	assert.JSONEq(t, `{"AvailabilityZoneID":"use1-az1","FutureField":{"x":1}}`, string(md.Raw),
		"the fields AWS adds later are kept")

	require.NotNil(t, got)
	assert.Equal(t, http.MethodGet, got.Method)
	assert.Equal(t, "/2026-01-15/metadata/execution-environment", got.URL.Path)
	assert.Equal(t, "Bearer "+token, got.Header.Get("Authorization"))
}

func TestGetCachesForTheLifeOfTheEnvironment(t *testing.T) {
	e := newEndpoint(t, answer(http.StatusOK, `{"AvailabilityZoneID":"euw1-az2"}`))
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)

	first, err := Get(t.Context())
	require.NoError(t, err)
	first.Raw[0] = 'X' // a caller scribbling on its copy

	second, err := Get(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "euw1-az2", second.AvailabilityZoneID)
	assert.JSONEq(t, `{"AvailabilityZoneID":"euw1-az2"}`, string(second.Raw), "the cache is not shared with callers")
	assert.Equal(t, int32(1), e.requests.Load(), "one request for the environment")

	ClearCache()
	_, err = Get(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(2), e.requests.Load(), "ClearCache forces a new request")
}

func TestGetOutsideLambda(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "no initialization type", env: map[string]string{env.LambdaInitializationType: ""}},
		{name: "POWERTOOLS_DEV", env: map[string]string{env.Dev: "true"}},
		{name: "POWERTOOLS_DEV as Powertools reads it", env: map[string]string{env.Dev: "yes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEndpoint(t, answer(http.StatusOK, `{"AvailabilityZoneID":"use1-az1"}`))
			t.Setenv(env.LambdaInitializationType, "on-demand")
			inLambda(t, e)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			md, err := Get(t.Context())
			require.NoError(t, err)
			assert.Equal(t, LambdaMetadata{}, md)
			assert.Zero(t, e.requests.Load(), "no request")
		})
	}
}

func TestGetFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		env     map[string]string
		opts    []Option
		status  int
		target  error
	}{
		{name: "no endpoint", env: map[string]string{env.LambdaMetadataAPI: ""}, target: ErrNoEndpoint},
		{name: "no token", env: map[string]string{env.LambdaMetadataToken: ""}, target: ErrNoToken},
		{name: "unauthorized", handler: answer(http.StatusUnauthorized, `{"message":"bad token"}`), status: 401, target: ErrStatus},
		{name: "server error", handler: answer(http.StatusInternalServerError, ""), status: 500, target: ErrStatus},
		{name: "not JSON", handler: answer(http.StatusOK, `<html>`), status: 200, target: ErrResponse},
		{name: "null", handler: answer(http.StatusOK, `null`), status: 200, target: ErrResponse},
		{name: "an array", handler: answer(http.StatusOK, `["use1-az1"]`), status: 200, target: ErrResponse},
		{name: "a wrong type", handler: answer(http.StatusOK, `{"AvailabilityZoneID":1}`), status: 200},
		{
			name: "too slow",
			handler: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
				answer(http.StatusOK, `{}`)(w, r)
			},
			opts:   []Option{WithTimeout(20 * time.Millisecond)},
			target: context.DeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := tt.handler
			if handler == nil {
				handler = answer(http.StatusOK, `{}`)
			}
			e := newEndpoint(t, handler)
			t.Setenv(env.LambdaInitializationType, "on-demand")
			inLambda(t, e)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			md, err := Get(t.Context(), tt.opts...)
			require.Error(t, err)
			assert.Equal(t, LambdaMetadata{}, md)
			mdErr, ok := errors.AsType[*LambdaMetadataError](err)
			require.True(t, ok, "every failure is a *LambdaMetadataError: %v", err)
			assert.Equal(t, tt.status, mdErr.StatusCode)
			if tt.target != nil {
				require.ErrorIs(t, err, tt.target)
			}
		})
	}
}

func TestGetUnreachableEndpoint(t *testing.T) {
	e := newEndpoint(t, answer(http.StatusOK, `{}`))
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)
	e.Close()

	_, err := Get(t.Context())
	mdErr, ok := errors.AsType[*LambdaMetadataError](err)
	require.True(t, ok)
	assert.Zero(t, mdErr.StatusCode, "no answer, no status")
}

func TestGetDoesNotCacheAFailure(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			answer(http.StatusInternalServerError, "")(w, r)
			return
		}
		answer(http.StatusOK, `{"AvailabilityZoneID":"use1-az1"}`)(w, r)
	})
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)

	_, err := Get(t.Context())
	require.ErrorIs(t, err, ErrStatus)

	fail.Store(false)
	md, err := Get(t.Context())
	require.NoError(t, err, "the next call asks again")
	assert.Equal(t, "use1-az1", md.AvailabilityZoneID)
}

func TestGetSharesOneRequestAmongConcurrentCalls(t *testing.T) {
	release := make(chan struct{})
	e := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		answer(http.StatusOK, `{"AvailabilityZoneID":"use1-az1"}`)(w, r)
	})
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)

	const callers = 50
	results := make([]LambdaMetadata, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { results[i], errs[i] = Get(t.Context()) })
	}
	require.Eventually(t, func() bool { return e.requests.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		assert.Equal(t, "use1-az1", results[i].AvailabilityZoneID)
	}
	assert.Equal(t, int32(1), e.requests.Load(), "one request for every concurrent caller")
}

func TestGetReturnsWhenItsContextEnds(t *testing.T) {
	release := make(chan struct{})
	e := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		answer(http.StatusOK, `{"AvailabilityZoneID":"use1-az1"}`)(w, r)
	})
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)

	impatient, cancel := context.WithCancel(t.Context())
	patient := make(chan LambdaMetadata, 1)
	go func() {
		md, err := Get(t.Context())
		assert.NoError(t, err)
		patient <- md
	}()
	require.Eventually(t, func() bool { return e.requests.Load() == 1 }, time.Second, time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := Get(impatient)
		done <- err
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled, "the caller that gives up returns at once")

	close(release)
	assert.Equal(t, "use1-az1", (<-patient).AvailabilityZoneID, "the others still get the answer")
}

func TestClearCacheDropsAFetchInFlight(t *testing.T) {
	release := make(chan struct{})
	e := newEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		answer(http.StatusOK, `{"AvailabilityZoneID":"stale"}`)(w, r)
	})
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)

	first := make(chan error, 1)
	go func() {
		_, err := Get(t.Context())
		first <- err
	}()
	require.Eventually(t, func() bool { return e.requests.Load() == 1 }, time.Second, time.Millisecond)
	ClearCache()
	close(release)
	require.NoError(t, <-first)

	cache.mu.Lock()
	cached := cache.value
	cache.mu.Unlock()
	assert.Nil(t, cached, "an answer to a request made before ClearCache is not kept")
}

func TestWithHTTPClient(t *testing.T) {
	e := newEndpoint(t, answer(http.StatusOK, `{"AvailabilityZoneID":"use1-az1"}`))
	t.Setenv(env.LambdaInitializationType, "on-demand")
	inLambda(t, e)
	var used atomic.Bool
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		return http.DefaultTransport.RoundTrip(r)
	})}

	_, err := Get(t.Context(), WithHTTPClient(client))
	require.NoError(t, err)
	assert.True(t, used.Load())
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLambdaMetadataErrorMessage(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "metadata: request failed with status 401: unexpected status",
		(&LambdaMetadataError{StatusCode: 401, Err: ErrStatus}).Error())
	assert.Equal(t, "metadata: environment variable AWS_LAMBDA_METADATA_API is not set",
		(&LambdaMetadataError{Err: ErrNoEndpoint}).Error())
}

func TestDecode(t *testing.T) {
	t.Parallel()
	md, err := decode([]byte(" {\"AvailabilityZoneID\":\"use1-az1\"}\n"))
	require.NoError(t, err)
	assert.Equal(t, "use1-az1", md.AvailabilityZoneID)
	assert.Equal(t, jsontext.Value(`{"AvailabilityZoneID":"use1-az1"}`), md.Raw)

	md, err = decode([]byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, md.AvailabilityZoneID, "a missing field stays empty")
}
