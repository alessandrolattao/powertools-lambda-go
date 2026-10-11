package oauth2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	xoauth2 "golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// retryDelays are the pauses before the two retries a transient failure
// allows.
var retryDelays = [...]time.Duration{100 * time.Millisecond, 200 * time.Millisecond}

// bearerToken is the b64token syntax of RFC 6750 an access token must
// follow to travel in an Authorization header.
var bearerToken = regexp.MustCompile(`^[-A-Za-z0-9._~+/]+=*$`)

// Why an attempt failed. They only reach the debug log: callers see
// ErrTokenExchange, never the provider's answer.
var (
	errSecret        = errors.New("the secret source failed or returned an empty secret")
	errStatus        = errors.New("the token endpoint did not answer 200")
	errFormResponse  = errors.New("the token endpoint answered a form instead of JSON")
	errInvalidToken  = errors.New("the token response is not a usable bearer token")
	errResponseBody  = errors.New("reading the token response failed")
	errBudgetExpired = errors.New("the acquisition budget ran out")
)

// acquire exchanges the client credentials for a token, retrying transport
// failures, 429 and 5xx answers twice, after 100 then 200 milliseconds, as
// long as the budget allows.
func (c *Client) acquire(ctx context.Context) (*accessToken, *Error) {
	for attempt := 0; ; attempt++ {
		token, retryable, err := c.attempt(ctx)
		if err == nil {
			return token, nil
		}
		debug.Logger().DebugContext(ctx, "oauth2: token exchange attempt failed", "attempt", attempt+1, "retryable", retryable, "err", err)
		if !retryable || attempt >= len(retryDelays) {
			return nil, errTokenExchange(retryable)
		}
		delay := retryDelays[attempt]
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return nil, errTokenExchange(true)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, errTokenExchange(true)
		}
	}
}

// attempt is one token request. retryable reports a failure worth another
// attempt.
func (c *Client) attempt(ctx context.Context) (token *accessToken, retryable bool, err error) {
	secret, err := c.secret(ctx)
	if err != nil || secret == "" {
		// The source's error may quote its configuration or the secret
		// store's answer: it does not cross this boundary.
		return nil, false, errSecret
	}
	cfg := &clientcredentials.Config{
		ClientID:       c.clientID,
		ClientSecret:   secret,
		TokenURL:       c.tokenURL,
		Scopes:         c.scopes,
		EndpointParams: c.params,
		AuthStyle:      c.style,
	}
	rec := &recorder{base: c.transport}
	client := &http.Client{Transport: rec, CheckRedirect: noRedirect}
	// The lifetime starts before the request: the provider counts it from
	// when it issues the token.
	started := c.now()
	result, err := cfg.Token(context.WithValue(ctx, xoauth2.HTTPClient, client))
	switch {
	case ctx.Err() != nil:
		return nil, true, errBudgetExpired
	case !rec.responded:
		return nil, true, fmt.Errorf("token request: %w", err)
	case rec.status != http.StatusOK:
		return nil, rec.status == http.StatusTooManyRequests || rec.status >= 500 && rec.status <= 599, errStatus
	case rec.readErr != nil:
		return nil, true, errResponseBody
	case err != nil:
		return nil, false, fmt.Errorf("token response: %w", err)
	case isForm(rec.contentType):
		return nil, false, errFormResponse
	}
	token, err = c.validate(result, started)
	return token, false, err
}

// validate applies Powertools' checks to the token response: a b64token
// access token, the bearer token type, and a positive, finite lifetime when
// there is one, not already over.
func (c *Client) validate(result *xoauth2.Token, started time.Time) (*accessToken, error) {
	if !bearerToken.MatchString(result.AccessToken) || !strings.EqualFold(result.TokenType, "bearer") {
		return nil, errInvalidToken
	}
	token := &accessToken{value: result.AccessToken}
	if raw := result.Extra("expires_in"); raw != nil {
		lifetime, ok := raw.(float64)
		if !ok || math.IsInf(lifetime, 0) || math.IsNaN(lifetime) || lifetime <= 0 {
			return nil, errInvalidToken
		}
		token.expiresAt = started.Add(time.Duration(lifetime * float64(time.Second)))
	}
	if !token.usable(c.now()) {
		return nil, errInvalidToken
	}
	return token, nil
}

// isForm reports the content types x/oauth2 parses as a form, which
// Powertools does not accept: a token response is JSON.
func isForm(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && (media == "application/x-www-form-urlencoded" || media == "text/plain")
}

// noRedirect stops a client at the first response: a redirect could take
// the credentials to a host nobody trusted.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// recorder notes what the token endpoint answered, which x/oauth2 does
// not tell apart: whether there was an answer at all, its status, its
// content type, and whether reading it failed.
type recorder struct {
	base        http.RoundTripper
	responded   bool
	status      int
	contentType string
	readErr     error
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err //nolint:wrapcheck // x/oauth2 sees the transport's own error; callers never do
	}
	r.responded = true
	r.status = resp.StatusCode
	r.contentType = resp.Header.Get("Content-Type")
	resp.Body = &recordedBody{ReadCloser: resp.Body, r: r}
	return resp, nil
}

type recordedBody struct {
	io.ReadCloser

	r *recorder
}

func (b *recordedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.r.readErr = err
	}
	return n, err //nolint:wrapcheck // an io.Reader returns io.EOF unwrapped
}
