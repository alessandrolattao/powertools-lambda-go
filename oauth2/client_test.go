package oauth2

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRefusesConfiguration(t *testing.T) {
	t.Parallel()
	const tokenURL = "https://idp.example.com/oauth2/token"
	secret := StaticSecret("s")
	cases := []struct {
		name     string
		tokenURL string
		clientID string
		secret   SecretSource
		opts     []Option
	}{
		{"http token URL", "http://idp.example.com/token", "c", secret, nil},
		{"token URL with user", "https://u@idp.example.com/token", "c", secret, nil},
		{"token URL with fragment", "https://idp.example.com/token#x", "c", secret, nil},
		{"blank client ID", tokenURL, " ", secret, nil},
		{"invalid UTF-8 client ID", tokenURL, "\xff", secret, nil},
		{"no secret source", tokenURL, "c", nil, nil},
		{"unknown auth method", tokenURL, "c", secret, []Option{WithAuthMethod("private_key_jwt")}},
		{"audience and resource", tokenURL, "c", secret, []Option{WithAudience("a"), WithResource("urn:a")}},
		{"invalid scope", tokenURL, "c", secret, []Option{WithScopes("a b")}},
		{"blank audience", tokenURL, "c", secret, []Option{WithAudience(" ")}},
		{"zero timeout", tokenURL, "c", secret, []Option{WithTimeout(0)}},
		{"negative request timeout", tokenURL, "c", secret, []Option{WithRequestTimeout(-time.Second)}},
		{"relative resource", tokenURL, "c", secret, []Option{WithResource("inventory")}},
		{"resource with fragment", tokenURL, "c", secret, []Option{WithResource("https://inventory.example.com/#x")}},
		{"resource with space", tokenURL, "c", secret, []Option{WithResource("https://inventory.example.com/a b")}},
		{"resource with broken escape", tokenURL, "c", secret, []Option{WithResource("https://inventory.example.com/%2")}},
		{"resource with digit scheme", tokenURL, "c", secret, []Option{WithResource("1https://inventory.example.com")}},
		{"resource with empty scheme", tokenURL, "c", secret, []Option{WithResource(":inventory")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.tokenURL, tc.clientID, tc.secret, tc.opts...)
			require.ErrorIs(t, err, ErrInvalidConfig)
			assert.NotContains(t, err.Error(), "example.com")
		})
	}

	t.Run("accepted resources", func(t *testing.T) {
		t.Parallel()
		for _, resource := range []string{"https://inventory.example.com", "urn:example:inventory", "https://inventory.example.com/a%2Fb?x=1", "Https+x.y-z:abc"} {
			_, err := New(tokenURL, "c", secret, WithResource(resource))
			require.NoError(t, err, resource)
		}
	})
}

func TestClientSecretBasic(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
	c, err := New(e.url(), "orders client", StaticSecret("s3cret+/:"), WithHTTPClient(e.server.Client()),
		WithScopes("inventory:read", "inventory:write", "inventory:read"), WithAudience("https://inventory.example.com"))
	require.NoError(t, err)

	header, err := c.AuthHeaders(t.Context())
	require.NoError(t, err)
	assert.Equal(t, http.Header{"Authorization": {"Bearer tok-1"}}, header)

	got := e.requests()
	require.Len(t, got, 1)
	credentials := url.QueryEscape("orders client") + ":" + url.QueryEscape("s3cret+/:")
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)), got[0].authorization)
	assert.Equal(t, "application/x-www-form-urlencoded", got[0].contentType)
	assert.Equal(t, url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"inventory:read inventory:write"},
		"audience":   {"https://inventory.example.com"},
	}, got[0].form)
}

func TestClientSecretPost(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
	c := e.newClient(t, WithAuthMethod(ClientSecretPost), WithResource("urn:example:inventory"))

	token, err := c.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-1", token)

	got := e.requests()
	require.Len(t, got, 1)
	assert.Empty(t, got[0].authorization)
	assert.Equal(t, url.Values{
		"grant_type":    {"client_credentials"},
		"resource":      {"urn:example:inventory"},
		"client_id":     {"orders"},
		"client_secret": {"s3cret"},
	}, got[0].form)
}

func TestTokenCaching(t *testing.T) {
	t.Parallel()
	clk := newClock()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"), tokenReply("tok-2", "3600"))
	c := e.newClient(t, withClock(clk.Now))

	for range 3 {
		token, err := c.Token(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "tok-1", token)
	}
	assert.Equal(t, int32(1), e.hits.Load(), "warm calls reuse the token")

	clk.Advance(time.Hour - 31*time.Second)
	token, err := c.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-1", token)

	clk.Advance(time.Second)
	token, err = c.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-2", token, "30 seconds before expiry, the token is replaced")
	assert.Equal(t, int32(2), e.hits.Load())
}

func TestShortLivedTokensAreNotCached(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		expiresIn string
		cached    bool
	}{
		"no lifetime": {"", false},
		"30 seconds":  {"30", false},
		"31 seconds":  {"31", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newTokenEndpoint(t, tokenReply("tok", tc.expiresIn))
			c := e.newClient(t, withClock(newClock().Now))
			for range 2 {
				token, err := c.Token(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "tok", token)
			}
			want := int32(2)
			if tc.cached {
				want = 1
			}
			assert.Equal(t, want, e.hits.Load())
		})
	}
}

func TestTokenResponses(t *testing.T) {
	t.Parallel()
	json := "application/json"
	cases := []struct {
		name      string
		reply     reply
		retryable bool
		hits      int32
	}{
		{"400", reply{http.StatusBadRequest, json, `{"error":"invalid_client"}`}, false, 1},
		{"401", reply{http.StatusUnauthorized, json, `{}`}, false, 1},
		{"201", reply{http.StatusCreated, json, `{"access_token":"a","token_type":"Bearer"}`}, false, 1},
		{"302", reply{http.StatusFound, json, `{}`}, false, 1},
		{"429", reply{http.StatusTooManyRequests, json, `{}`}, true, 3},
		{"500", reply{http.StatusInternalServerError, json, `{}`}, true, 3},
		{"503", reply{http.StatusServiceUnavailable, "", ``}, true, 3},
		{"not JSON", reply{http.StatusOK, json, `<html>`}, false, 1},
		{"JSON array", reply{http.StatusOK, json, `[]`}, false, 1},
		{"form", reply{http.StatusOK, "application/x-www-form-urlencoded", `access_token=a&token_type=bearer`}, false, 1},
		{"text", reply{http.StatusOK, "text/plain; charset=utf-8", `access_token=a&token_type=bearer`}, false, 1},
		{"no access token", reply{http.StatusOK, json, `{"token_type":"Bearer"}`}, false, 1},
		{"access token number", reply{http.StatusOK, json, `{"access_token":1,"token_type":"Bearer"}`}, false, 1},
		{"access token with space", reply{http.StatusOK, json, `{"access_token":"a b","token_type":"Bearer"}`}, false, 1},
		{"access token with quote", reply{http.StatusOK, json, `{"access_token":"a\"","token_type":"Bearer"}`}, false, 1},
		{"no token type", reply{http.StatusOK, json, `{"access_token":"a"}`}, false, 1},
		{"MAC token", reply{http.StatusOK, json, `{"access_token":"a","token_type":"mac"}`}, false, 1},
		{"lifetime string", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","expires_in":"3600"}`}, false, 1},
		{"lifetime zero", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","expires_in":0}`}, false, 1},
		{"lifetime negative", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","expires_in":-1}`}, false, 1},
		{"lifetime with a fraction", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","expires_in":3600.5}`}, false, 1},
		{"lifetime boolean", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","expires_in":true}`}, false, 1},
		{"error with 200", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","error":"x"}`}, false, 1},
		{"larger than 1MiB", reply{http.StatusOK, json, `{"access_token":"a","token_type":"Bearer","pad":"` + strings.Repeat("x", 1<<20) + `"}`}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTokenEndpoint(t, tc.reply)
			_, err := e.newClient(t).Token(t.Context())
			requireExchangeError(t, err, tc.retryable)
			assert.Equal(t, tc.hits, e.hits.Load())
			assert.NotContains(t, err.Error(), "s3cret")
		})
	}

	t.Run("accepted variations", func(t *testing.T) {
		t.Parallel()
		for _, body := range []string{
			`{"access_token":"a.b-c_d~e+f/g==","token_type":"bearer"}`,
			`{"access_token":"a","token_type":"BEARER","scope":"x","refresh_token":"r"}`,
		} {
			e := newTokenEndpoint(t, reply{http.StatusOK, json, body})
			_, err := e.newClient(t).Token(t.Context())
			require.NoError(t, err, body)
		}
	})
}

func TestRetries(t *testing.T) {
	t.Parallel()
	failure := reply{http.StatusBadGateway, "", ""}
	var lookups atomic.Int32
	e := newTokenEndpoint(t, failure, failure, tokenReply("tok", "3600"))
	c, err := New(e.url(), "orders", countingSecret("s3cret", &lookups), WithHTTPClient(e.server.Client()))
	require.NoError(t, err)

	start := time.Now()
	token, err := c.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok", token)
	assert.Equal(t, int32(3), e.hits.Load())
	assert.Equal(t, int32(3), lookups.Load(), "the secret is looked up for every attempt")
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "100 then 200 milliseconds of backoff")
}

func TestRetriesStayWithinTheBudget(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	failing := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody, Request: req}, nil
	})
	c, err := New("https://idp.example.com/token", "orders", StaticSecret("s3cret"),
		WithHTTPClient(&http.Client{Transport: failing}), WithTimeout(90*time.Millisecond))
	require.NoError(t, err)
	_, err = c.Token(t.Context())
	requireExchangeError(t, err, true)
	assert.Equal(t, int32(1), hits.Load(), "no retry when the backoff would exceed the budget")
}

func TestTransportFailureIsRetryable(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok", "3600"))
	c := e.newClient(t)
	e.server.Close()
	_, err := c.Token(t.Context())
	requireExchangeError(t, err, true)
}

func TestSlowEndpointExhaustsTheBudget(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok", "3600"))
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	e.set(func(e *tokenEndpoint) { e.gate = gate })
	c := e.newClient(t, WithTimeout(200*time.Millisecond))
	start := time.Now()
	_, err := c.Token(t.Context())
	requireExchangeError(t, err, true)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestSecretSourceFailures(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]SecretSource{
		"error": func(context.Context) (string, error) {
			return "", errors.New("AccessDeniedException: arn:aws:secretsmanager:eu-west-1:123456789012:secret:s3cret")
		},
		"empty": StaticSecret(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newTokenEndpoint(t, tokenReply("tok", "3600"))
			c, err := New(e.url(), "orders", source, WithHTTPClient(e.server.Client()))
			require.NoError(t, err)
			_, err = c.Token(t.Context())
			requireExchangeError(t, err, false)
			assert.Equal(t, int32(0), e.hits.Load())
			assert.NotContains(t, err.Error(), "s3cret")
			require.NoError(t, errors.Unwrap(err), "the cause is dropped")
		})
	}
}

func TestExpiredOnArrival(t *testing.T) {
	t.Parallel()
	clk := newClock()
	e := newTokenEndpoint(t, tokenReply("tok", "5"))
	e.set(func(e *tokenEndpoint) { e.before = func() { clk.Advance(5 * time.Second) } })
	_, err := e.newClient(t, withClock(clk.Now)).Token(t.Context())
	requireExchangeError(t, err, false)
}

func TestConcurrentCallersShareOneExchange(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply reply
		ok    bool
	}{
		"success":         {tokenReply("tok", "3600"), true},
		"short-lived":     {tokenReply("tok", "10"), true},
		"refused":         {reply{http.StatusBadRequest, "", ""}, false},
		"without expires": {tokenReply("tok", ""), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newTokenEndpoint(t, tc.reply)
			gate := make(chan struct{})
			e.set(func(e *tokenEndpoint) { e.gate = gate })
			c := e.newClient(t, WithTimeout(10*time.Second))

			var wg sync.WaitGroup
			results := make(chan error, 10)
			for range cap(results) {
				wg.Go(func() {
					token, err := c.Token(t.Context())
					if err == nil && token != "tok" {
						err = fmt.Errorf("token %q", token)
					}
					results <- err
				})
			}
			require.Eventually(t, func() bool { return e.hits.Load() == 1 }, 5*time.Second, time.Millisecond)
			time.Sleep(50 * time.Millisecond)
			close(gate)
			wg.Wait()
			close(results)
			for err := range results {
				if tc.ok {
					require.NoError(t, err)
				} else {
					requireExchangeError(t, err, false)
				}
			}
			assert.Equal(t, int32(1), e.hits.Load(), "one exchange for every concurrent caller")
		})
	}
}

func TestWaiterKeepsItsOwnDeadline(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok", "3600"))
	gate := make(chan struct{})
	e.set(func(e *tokenEndpoint) { e.gate = gate })
	c := e.newClient(t, WithTimeout(10*time.Second))

	owner := make(chan error)
	go func() {
		_, err := c.Token(t.Context())
		owner <- err
	}()
	require.Eventually(t, func() bool { return e.hits.Load() == 1 }, 5*time.Second, time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Token(ctx)
	requireExchangeError(t, err, true)

	close(gate)
	require.NoError(t, <-owner)
}

func TestAbandonedExchangeIsRetried(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok", "3600"))
	gate := make(chan struct{})
	e.set(func(e *tokenEndpoint) { e.gate = gate })
	c := e.newClient(t, WithTimeout(10*time.Second))

	ctx, cancel := context.WithCancel(t.Context())
	owner := make(chan error)
	go func() {
		_, err := c.Token(ctx)
		owner <- err
	}()
	require.Eventually(t, func() bool { return e.hits.Load() == 1 }, 5*time.Second, time.Millisecond)

	waiter := make(chan error)
	go func() {
		_, err := c.Token(t.Context())
		waiter <- err
	}()
	time.Sleep(50 * time.Millisecond)
	// Only the first request waits at the gate.
	e.set(func(e *tokenEndpoint) { e.gate = nil })
	cancel()
	requireExchangeError(t, <-owner, true)
	require.NoError(t, <-waiter, "the waiter exchanges again instead of sharing the abandoned failure")
	assert.Equal(t, int32(2), e.hits.Load())
	close(gate)
}

func TestClientHidesItsConfiguration(t *testing.T) {
	t.Parallel()
	c, err := New("https://idp.example.com/token", "orders", StaticSecret("s3cret"))
	require.NoError(t, err)
	assert.Equal(t, "<OAuth2Client>", fmt.Sprintf("%+v", c))
}

func TestErrors(t *testing.T) {
	t.Parallel()
	exchange := fmt.Errorf("wrapped: %w", errTokenExchange(true))
	require.ErrorIs(t, exchange, ErrTokenExchange)
	require.NotErrorIs(t, exchange, ErrDownstreamRequest)
	assert.Equal(t, "unable to acquire an access token", errTokenExchange(false).Error())

	downstream := fmt.Errorf("wrapped: %w", errDownstream())
	require.ErrorIs(t, downstream, ErrDownstreamRequest)
	require.NotErrorIs(t, downstream, ErrTokenExchange)
	e, ok := errors.AsType[*Error](downstream)
	require.True(t, ok)
	assert.Equal(t, ReasonDownstreamRequestFailed, e.Reason)
	assert.False(t, e.Retryable)
	assert.Equal(t, "authenticated request failed", e.Error())
}
