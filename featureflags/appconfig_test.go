package featureflags

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// fakeAppConfig answers every poll with its current document.
type fakeAppConfig struct {
	mu    sync.Mutex
	doc   string
	err   error
	polls int
}

func (f *fakeAppConfig) StartConfigurationSession(context.Context, *appconfigdata.StartConfigurationSessionInput, ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &appconfigdata.StartConfigurationSessionOutput{InitialConfigurationToken: aws.String("token")}, nil
}

func (f *fakeAppConfig) GetLatestConfiguration(context.Context, *appconfigdata.GetLatestConfigurationInput, ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return &appconfigdata.GetLatestConfigurationOutput{Configuration: []byte(f.doc), NextPollConfigurationToken: aws.String("token")}, nil
}

func (f *fakeAppConfig) set(doc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doc = doc
}

const appDoc = `{"log_level":"INFO","app":{"features":{
	"premium":{"default":false,"rules":{
		"z rule":{"when_match":true,"conditions":[{"action":"EQUALS","key":"tier","value":"premium"}]},
		"a rule":{"when_match":false,"conditions":[{"action":"EQUALS","key":"tier","value":"premium"}]}}},
	"split":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"MODULO_RANGE","key":"id","value":{"BASE":10,"START":0,"END":4}}]}}},
	"limits":{"default":[3,1],"boolean_type":false}}}}`

func TestAppConfigStore(t *testing.T) {
	t.Parallel()
	fake := &fakeAppConfig{doc: appDoc}
	store := NewAppConfigStore(fake, "app", "prod", "features", WithEnvelope("app.features"), WithMaxAge(time.Hour))
	flags := New(store)
	ctx := t.Context()

	premium, err := flags.Evaluate(ctx, "premium", map[string]any{"tier": "premium"}, false)
	require.NoError(t, err)
	assert.True(t, premium, "the rules keep their document order through the envelope")

	split, err := flags.Evaluate(ctx, "split", map[string]any{"id": 13}, false)
	require.NoError(t, err)
	assert.True(t, split, "integers survive the envelope, so MODULO_RANGE validates")

	limits, err := flags.Evaluate(ctx, "limits", nil, []int{})
	require.NoError(t, err)
	assert.Equal(t, []int{3, 1}, limits)

	raw, err := store.GetRawConfiguration(ctx)
	require.NoError(t, err)
	assert.JSONEq(t, appDoc, string(raw))

	doc, err := store.GetConfiguration(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(doc), `{"premium":{"default":false,"rules":{"z rule"`)
	assert.Equal(t, 1, fake.polls, "the document is cached for the max age")
}

func TestAppConfigStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	denied := NewAppConfigStore(&fakeAppConfig{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "no"}}, "app", "prod", "features")
	_, err := denied.GetConfiguration(ctx)
	require.ErrorIs(t, err, ErrStoreClient)
	_, err = New(denied).Evaluate(ctx, "x", nil, false)
	require.ErrorIs(t, err, ErrStoreClient, "an access denied reaches the caller")

	down := NewAppConfigStore(&fakeAppConfig{err: errors.New("connection refused")}, "app", "prod", "features")
	_, err = down.GetConfiguration(ctx)
	require.ErrorIs(t, err, ErrConfigurationStore)
	got, err := New(down).Evaluate(ctx, "x", nil, true)
	require.NoError(t, err)
	assert.True(t, got, "an unavailable store gives the default")

	notJSON := NewAppConfigStore(&fakeAppConfig{doc: "not json"}, "app", "prod", "features")
	_, err = notJSON.GetConfiguration(ctx)
	require.ErrorIs(t, err, ErrConfigurationStore)

	badEnvelope := NewAppConfigStore(&fakeAppConfig{doc: appDoc}, "app", "prod", "features", WithEnvelope("app.["))
	_, err = badEnvelope.GetConfiguration(ctx)
	require.ErrorIs(t, err, jmespath.ErrInvalidEnvelope)
	_, err = New(badEnvelope).Evaluate(ctx, "x", nil, true)
	require.ErrorIs(t, err, jmespath.ErrInvalidEnvelope, "as in Python, an envelope error is not swallowed")
}

func TestAppConfigStoreEnvelopeFunctions(t *testing.T) {
	t.Parallel()
	fake := &fakeAppConfig{doc: `{"wrapped":"{\"b\":{\"default\":true},\"a\":{\"default\":false}}","other":{"x":{"default":true}}}`}
	upper := jmespath.Function{
		Name: "pick",
		Args: []jmespath.Arg{{Types: []jmespath.Type{jmespath.TypeObject}}},
		Call: func(args []any) (any, error) { return args[0], nil },
	}
	store := NewAppConfigStore(fake, "app", "prod", "features", WithEnvelope("powertools_json(wrapped)"), WithJMESPathFunctions(upper), WithMaxAge(0))
	doc, err := store.GetConfiguration(t.Context())
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":{"default":false},"b":{"default":true}}`, string(doc))
	assert.True(t, strings.HasPrefix(string(doc), `{"a"`), "objects the expression builds come in key order")

	custom := NewAppConfigStore(fake, "app", "prod", "features", WithEnvelope("pick(other)"), WithJMESPathFunctions(upper), WithMaxAge(0))
	doc, err = custom.GetConfiguration(t.Context())
	require.NoError(t, err)
	assert.JSONEq(t, `{"x":{"default":true}}`, string(doc))

	fake.set(`{"wrapped":"{}","other":{"y":{"default":false}}}`)
	doc, err = custom.GetConfiguration(t.Context())
	require.NoError(t, err)
	assert.JSONEq(t, `{"y":{"default":false}}`, string(doc), "a new document is extracted again")
}

func TestAppConfigStoreFromConfig(t *testing.T) {
	t.Parallel()
	store := AppConfigStoreFromConfig(aws.Config{Region: "eu-west-1"}, "app", "prod", "features")
	assert.Equal(t, "app", store.provider.Application())
	assert.Equal(t, DefaultMaxAge, store.maxAge)
}
