package appconfig

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// fakeAppConfig hands out single-use tokens and answers each poll with the
// next configuration of its script ("" meaning no change).
type fakeAppConfig struct {
	mu       sync.Mutex
	script   []string
	interval int32
	expired  map[string]bool
	starts   []appconfigdata.StartConfigurationSessionInput
	polls    []string
	issued   int
	used     map[string]bool
}

func (f *fakeAppConfig) StartConfigurationSession(_ context.Context, in *appconfigdata.StartConfigurationSessionInput, _ ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, *in)
	return &appconfigdata.StartConfigurationSessionOutput{InitialConfigurationToken: aws.String(f.token())}, nil
}

func (f *fakeAppConfig) token() string {
	f.issued++
	return "token-" + strconv.Itoa(f.issued)
}

func (f *fakeAppConfig) GetLatestConfiguration(_ context.Context, in *appconfigdata.GetLatestConfigurationInput, _ ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := aws.ToString(in.ConfigurationToken)
	if f.used == nil {
		f.used = make(map[string]bool)
	}
	if f.used[token] || f.expired[token] {
		return nil, &types.BadRequestException{Message: aws.String("invalid token")}
	}
	f.used[token] = true
	f.polls = append(f.polls, token)
	var body string
	if len(f.script) > 0 {
		body, f.script = f.script[0], f.script[1:]
	}
	return &appconfigdata.GetLatestConfigurationOutput{
		Configuration:              []byte(body),
		NextPollConfigurationToken: aws.String(f.token()),
		NextPollIntervalInSeconds:  f.interval,
	}, nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestProvider(f *fakeAppConfig) (*Provider, *fakeClock) {
	p := New(f, "app", "prod")
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	p.Provider = parameters.NewProvider(&store{client: f, application: "app", environment: "prod", now: clock.Now, sessions: make(map[string]*session)})
	return p, clock
}

func TestSessionAndTokens(t *testing.T) {
	t.Parallel()
	f := &fakeAppConfig{script: []string{`{"v":1}`, "", `{"v":2}`}}
	p, _ := newTestProvider(f)
	ctx := t.Context()
	read := func() string {
		v, err := p.Get(ctx, "features", parameters.WithMaxAge(0), WithMinimumPollInterval(30*time.Second))
		require.NoError(t, err)
		return v
	}
	assert.JSONEq(t, `{"v":1}`, read())
	assert.JSONEq(t, `{"v":1}`, read(), "an empty answer means no change: the last configuration is kept")
	assert.JSONEq(t, `{"v":2}`, read())

	require.Len(t, f.starts, 1, "one session per profile")
	start := f.starts[0]
	assert.Equal(t, "app", aws.ToString(start.ApplicationIdentifier))
	assert.Equal(t, "prod", aws.ToString(start.EnvironmentIdentifier))
	assert.Equal(t, "features", aws.ToString(start.ConfigurationProfileIdentifier))
	assert.Equal(t, int32(30), aws.ToInt32(start.RequiredMinimumPollIntervalInSeconds))
	assert.Equal(t, []string{"token-1", "token-2", "token-3"}, f.polls, "each poll uses the token of the previous one")
}

func TestPollIntervalIsHonored(t *testing.T) {
	t.Parallel()
	f := &fakeAppConfig{script: []string{"one", "two"}, interval: 60}
	p, clock := newTestProvider(f)
	read := func() string {
		v, err := p.Get(t.Context(), "profile", parameters.WithForceFetch(true))
		require.NoError(t, err)
		return v
	}
	assert.Equal(t, "one", read())
	clock.Advance(59 * time.Second)
	assert.Equal(t, "one", read(), "AppConfig asked not to be polled before 60 seconds")
	assert.Len(t, f.polls, 1)
	clock.Advance(time.Second)
	assert.Equal(t, "two", read())
	assert.Len(t, f.polls, 2)
}

func TestExpiredTokenStartsANewSession(t *testing.T) {
	t.Parallel()
	f := &fakeAppConfig{script: []string{"one", "two"}, expired: map[string]bool{"token-2": true}}
	p, _ := newTestProvider(f)
	_, err := p.Get(t.Context(), "profile", parameters.WithMaxAge(0))
	require.NoError(t, err)
	v, err := p.Get(t.Context(), "profile", parameters.WithMaxAge(0))
	require.NoError(t, err)
	assert.Equal(t, "two", v)
	assert.Len(t, f.starts, 2, "the refused token was replaced by a new session")
}

func TestFirstEmptyConfiguration(t *testing.T) {
	t.Parallel()
	p, _ := newTestProvider(&fakeAppConfig{script: []string{""}})
	v, err := p.Get(t.Context(), "empty")
	require.NoError(t, err)
	assert.Empty(t, v)
}

func TestProfilesHaveTheirOwnSessions(t *testing.T) {
	t.Parallel()
	f := &fakeAppConfig{script: []string{"a", "b"}}
	p, _ := newTestProvider(f)
	a, err := p.Get(t.Context(), "first")
	require.NoError(t, err)
	b, err := p.Get(t.Context(), "second")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, []string{a, b})
	assert.Len(t, f.starts, 2)
}

func TestConcurrentPollsUseEachTokenOnce(t *testing.T) {
	t.Parallel()
	script := make([]string, 64)
	for i := range script {
		script[i] = strconv.Itoa(i)
	}
	f := &fakeAppConfig{script: script}
	p, _ := newTestProvider(f)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			// Different transforms are different cache keys, so these polls
			// really run concurrently.
			transform := []parameters.Transform{parameters.TransformNone, parameters.TransformJSON}[i%2]
			_, err := parameters.Get[any](t.Context(), p, "profile", parameters.WithMaxAge(0), parameters.WithTransform(transform))
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.Len(t, f.starts, 1)
}

func TestGetMultipleIsUnsupported(t *testing.T) {
	t.Parallel()
	p := New(&fakeAppConfig{}, "app", "prod")
	_, err := p.GetMultiple(t.Context(), "/path")
	require.ErrorIs(t, err, errors.ErrUnsupported)
	require.ErrorIs(t, err, parameters.ErrGetParameter)
}

func TestApplicationDefaultsToTheService(t *testing.T) {
	t.Setenv(env.ServiceName, "")
	assert.Equal(t, "service_undefined", New(&fakeAppConfig{}, "", "prod").Application())
	t.Setenv(env.ServiceName, "payments")
	p := New(&fakeAppConfig{}, "", "prod")
	assert.Equal(t, "payments", p.Application())
	assert.Equal(t, "prod", p.Environment())
}

func TestDefaultsPerApplicationAndEnvironment(t *testing.T) {
	t.Parallel()
	var d core.Defaults[[2]string, *Provider]
	built := 0
	get := func(app, environment string) *Provider {
		p, err := d.Get([2]string{app, environment}, func() (*Provider, error) {
			built++
			return New(&fakeAppConfig{}, app, environment), nil
		})
		require.NoError(t, err)
		return p
	}
	a := get("app-a", "prod")
	b := get("app-b", "prod")
	c := get("app-a", "dev")
	assert.Same(t, a, get("app-a", "prod"))
	assert.Equal(t, 3, built, "Python reuses the first provider for every application and environment")
	assert.Equal(t, "app-b", b.Application())
	assert.Equal(t, "dev", c.Environment())
}
