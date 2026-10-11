package appconfig

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata/types"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// Client is the part of the AppConfigData client the provider calls;
// *appconfigdata.Client satisfies it.
type Client interface {
	StartConfigurationSession(ctx context.Context, params *appconfigdata.StartConfigurationSessionInput, optFns ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error)
	GetLatestConfiguration(ctx context.Context, params *appconfigdata.GetLatestConfigurationInput, optFns ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error)
}

// Provider reads the configuration profiles of one AppConfig application
// and environment. The name of a read is the configuration profile (name or
// id). It is safe for concurrent use.
type Provider struct {
	*parameters.Provider

	application string
	environment string
}

// New returns a provider of the profiles of application in environment.
// An empty application means POWERTOOLS_SERVICE_NAME, "service_undefined"
// when that is unset, as in Python.
func New(client Client, application, environment string) *Provider {
	application = resolveApplication(application)
	s := &store{
		client:      client,
		application: application,
		environment: environment,
		now:         time.Now,
		sessions:    make(map[string]*session),
	}
	return &Provider{Provider: parameters.NewProvider(s), application: application, environment: environment}
}

// FromConfig returns a provider with an AppConfigData client built from cfg.
func FromConfig(cfg aws.Config, application, environment string) *Provider {
	return New(appconfigdata.NewFromConfig(cfg), application, environment)
}

// Application returns the application the provider reads.
func (p *Provider) Application() string { return p.application }

// Environment returns the environment the provider reads.
func (p *Provider) Environment() string { return p.environment }

// readOptions are the AppConfig options of one read.
type readOptions struct {
	minimumPollInterval *int32
}

// WithMinimumPollInterval sets RequiredMinimumPollIntervalInSeconds on the
// configuration session: AppConfig then refuses polls closer than d. It
// applies when the session of a profile starts, on its first read, as
// Python's sdk_options do. Whole seconds; at least 15 seconds per AppConfig.
func WithMinimumPollInterval(d time.Duration) parameters.Option {
	seconds := int32(min(d/time.Second, 1<<31-1)) //nolint:gosec // G115: clamped to the int32 range on the line itself
	return parameters.WithStoreOption("", func(o *readOptions) { o.minimumPollInterval = &seconds })
}

// The sentinel errors of the provider.
var (
	// ErrNoToken reports a response of AppConfig without the token of the
	// next poll, which the session cannot go on without.
	ErrNoToken = errors.New("appconfig returned no configuration token")
)

// store keeps one AppConfigData session per configuration profile.
type store struct {
	client      Client
	application string
	environment string
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
}

// session is the state of the configuration session of one profile.
// AppConfig tokens are single use, so the polls of one profile are
// serialized by its mutex.
type session struct {
	mu       sync.Mutex
	token    string
	nextPoll time.Time
	last     []byte
	hasLast  bool
}

func (s *store) session(profile string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[profile]
	if !ok {
		sess = &session{}
		s.sessions[profile] = sess
	}
	return sess
}

// Get returns the latest configuration of profile.
//
// It starts the profile's session on first use and polls it with the
// session's token afterwards. AppConfigData answers an empty configuration
// when nothing changed since the last poll: the last configuration is then
// returned again. Polls sooner than the interval AppConfig asked for
// (NextPollIntervalInSeconds) return the last configuration without calling
// AppConfig. A token AppConfig refuses (they expire after 24 hours) starts a
// new session, once.
func (s *store) Get(ctx context.Context, profile string, opts parameters.StoreOptions) ([]byte, error) {
	var o readOptions
	opts.Apply(&o)
	sess := s.session(profile)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if sess.hasLast && s.now().Before(sess.nextPoll) {
		return sess.last, nil
	}
	resumed := sess.token != ""
	out, err := s.poll(ctx, profile, sess, &o)
	if resumed && errors.As(err, new(*types.BadRequestException)) {
		// A session that worked before refuses its token: it expired, or
		// a failed call consumed it. A new session has a new one.
		sess.token = ""
		out, err = s.poll(ctx, profile, sess, &o)
	}
	if err != nil {
		return nil, err
	}
	sess.nextPoll = s.now().Add(time.Duration(out.NextPollIntervalInSeconds) * time.Second)
	if len(out.Configuration) > 0 || !sess.hasLast {
		sess.last = out.Configuration
		sess.hasLast = true
	}
	return sess.last, nil
}

// poll starts the session when it has no token, then asks for the latest
// configuration and keeps the next token.
func (s *store) poll(ctx context.Context, profile string, sess *session, o *readOptions) (*appconfigdata.GetLatestConfigurationOutput, error) {
	if sess.token == "" {
		started, err := s.client.StartConfigurationSession(ctx, &appconfigdata.StartConfigurationSessionInput{
			ApplicationIdentifier:                aws.String(s.application),
			EnvironmentIdentifier:                aws.String(s.environment),
			ConfigurationProfileIdentifier:       aws.String(profile),
			RequiredMinimumPollIntervalInSeconds: o.minimumPollInterval,
		})
		if err != nil {
			return nil, fmt.Errorf("start configuration session: %w", err)
		}
		if aws.ToString(started.InitialConfigurationToken) == "" {
			return nil, ErrNoToken
		}
		sess.token = aws.ToString(started.InitialConfigurationToken)
	}
	out, err := s.client.GetLatestConfiguration(ctx, &appconfigdata.GetLatestConfigurationInput{
		ConfigurationToken: aws.String(sess.token),
	})
	if err != nil {
		return nil, fmt.Errorf("get latest configuration: %w", err)
	}
	if next := aws.ToString(out.NextPollConfigurationToken); next != "" {
		sess.token = next
	} else {
		sess.token = ""
	}
	return out, nil
}

// GetMultiple is not supported: AppConfig has no paths, as in Python.
func (*store) GetMultiple(context.Context, string, parameters.StoreOptions) (map[string][]byte, error) {
	return nil, fmt.Errorf("appconfig reads one configuration profile at a time: %w", errors.ErrUnsupported)
}

func resolveApplication(application string) string {
	if application != "" {
		return application
	}
	return env.ServiceNameOr(env.DefaultServiceName)
}

// defaults are the providers GetAppConfig builds, one per application and
// environment: Python keeps only the first one it builds, whatever
// application and environment later calls name.
var defaults core.Defaults[[2]string, *Provider]

func init() {
	core.RegisterDefaults(defaults.Reset)
}

// GetAppConfig returns the latest configuration of profile name, in
// environment of application, as a T (see parameters.Get), through a
// provider built on first use from the default AWS configuration: Python's
// get_app_config. An empty application means POWERTOOLS_SERVICE_NAME.
//
// Each application and environment gets its own provider, so calls naming
// different ones never read each other's configuration.
func GetAppConfig[T any](ctx context.Context, name, environment, application string, opts ...parameters.Option) (T, error) {
	application = resolveApplication(application)
	p, err := defaults.Get([2]string{application, environment}, func() (*Provider, error) {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("load the default AWS configuration: %w", err)
		}
		return FromConfig(cfg, application, environment), nil
	})
	if err != nil {
		var zero T
		return zero, &parameters.GetParameterError{Name: name, Err: err}
	}
	return parameters.Get[T](ctx, p, name, opts...)
}
