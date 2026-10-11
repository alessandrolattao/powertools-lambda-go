package featureflags

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/appconfig"
)

// DefaultMaxAge is how long AppConfigStore keeps a configuration before
// asking AppConfig again, as Python's max_age=5.
const DefaultMaxAge = 5 * time.Second

// AppConfigStore reads the feature flags schema from an AWS AppConfig
// freeform configuration profile: Python's AppConfigStore. It reads through
// a parameters/appconfig provider, so it keeps the AppConfigData session of
// the profile and honors the poll interval AppConfig asks for. It is safe
// for concurrent use.
//
// The AppConfig "Feature Flags" profile type (AWS.AppConfig.FeatureFlags)
// has another format and is not supported, as in Python.
type AppConfigStore struct {
	provider  *appconfig.Provider
	profile   string
	maxAge    time.Duration
	envelope  string
	functions []jmespath.Function

	mu            sync.Mutex
	lastRaw       []byte
	lastExtracted []byte
}

// AppConfigStoreOption configures an AppConfigStore.
type AppConfigStoreOption func(*AppConfigStore)

// WithMaxAge keeps the configuration for d before asking AppConfig again,
// DefaultMaxAge (five seconds) by default. AppConfig is never polled sooner
// than the poll interval of the session, whatever d says.
func WithMaxAge(d time.Duration) AppConfigStoreOption {
	return func(s *AppConfigStore) { s.maxAge = d }
}

// WithEnvelope selects the feature flags in a larger document with a
// JMESPath expression, for example "features" or "app.features". The
// Powertools JMESPath functions (powertools_json, powertools_base64,
// powertools_base64_gzip) are available.
func WithEnvelope(expression string) AppConfigStoreOption {
	return func(s *AppConfigStore) { s.envelope = expression }
}

// WithJMESPathFunctions makes custom functions available to the envelope,
// Python's jmespath_options. The Powertools functions stay available.
func WithJMESPathFunctions(functions ...jmespath.Function) AppConfigStoreOption {
	return func(s *AppConfigStore) { s.functions = append(s.functions, functions...) }
}

// NewAppConfigStore returns a store that reads a configuration profile of
// application in environment through client.
// An empty application means POWERTOOLS_SERVICE_NAME, as in
// parameters/appconfig.
func NewAppConfigStore(client appconfig.Client, application, environment, profile string, opts ...AppConfigStoreOption) *AppConfigStore {
	return newAppConfigStore(appconfig.New(client, application, environment), profile, opts)
}

// AppConfigStoreFromConfig returns a store with an AppConfigData client
// built from cfg.
func AppConfigStoreFromConfig(cfg aws.Config, application, environment, profile string, opts ...AppConfigStoreOption) *AppConfigStore {
	return newAppConfigStore(appconfig.FromConfig(cfg, application, environment), profile, opts)
}

func newAppConfigStore(provider *appconfig.Provider, profile string, opts []AppConfigStoreOption) *AppConfigStore {
	s := &AppConfigStore{provider: provider, profile: profile, maxAge: DefaultMaxAge}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// GetRawConfiguration returns the whole configuration document, before the
// envelope: Python's get_raw_configuration. An access denied is a
// *StoreClientError, any other failure (AppConfig unreachable, a document
// that is not JSON) a *ConfigurationStoreError.
func (s *AppConfigStore) GetRawConfiguration(ctx context.Context) (jsontext.Value, error) {
	raw, err := parameters.Get[jsontext.Value](ctx, s.provider, s.profile,
		parameters.WithTransform(parameters.TransformJSON),
		parameters.WithMaxAge(s.maxAge),
	)
	if err != nil {
		if apiErr, ok := errors.AsType[smithy.APIError](err); ok && strings.Contains(apiErr.ErrorCode(), "AccessDenied") {
			return nil, &StoreClientError{Err: err}
		}
		return nil, &ConfigurationStoreError{Message: "Unable to get AWS AppConfig configuration file", Err: err}
	}
	return raw, nil
}

// GetConfiguration returns the feature flags: the configuration document,
// or what the envelope selects in it. The selection is computed once per
// version of the document. An envelope that cannot be compiled or evaluated
// returns a *jmespath.EnvelopeError, which Evaluate passes on.
func (s *AppConfigStore) GetConfiguration(ctx context.Context) (jsontext.Value, error) {
	raw, err := s.GetRawConfiguration(ctx)
	if err != nil || s.envelope == "" {
		return raw, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastExtracted != nil && bytes.Equal(raw, s.lastRaw) {
		return bytes.Clone(s.lastExtracted), nil
	}
	if _, compileErr := jmespath.Compile(s.envelope, jmespath.WithFunctions(s.functions...)); compileErr != nil {
		return nil, compileErr
	}
	extracted, err := extract(raw, s.envelope, s.functions)
	if err != nil {
		return nil, err
	}
	s.lastRaw, s.lastExtracted = bytes.Clone(raw), extracted
	return bytes.Clone(extracted), nil
}
