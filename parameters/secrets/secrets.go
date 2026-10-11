package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// Client is the part of the Secrets Manager client the provider calls;
// *secretsmanager.Client satisfies it.
type Client interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	BatchGetSecretValue(ctx context.Context, params *secretsmanager.BatchGetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.BatchGetSecretValueOutput, error)
	PutSecretValue(ctx context.Context, params *secretsmanager.PutSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	CreateSecret(ctx context.Context, params *secretsmanager.CreateSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
}

// Provider reads and writes Secrets Manager secrets. It is safe for
// concurrent use: create it once and share it across invocations.
type Provider struct {
	*parameters.Provider

	client Client
	cache  *core.Cache
}

// New returns a provider that calls Secrets Manager through client.
func New(client Client) *Provider {
	s := &store{client: client, cache: core.NewCache()}
	return &Provider{Provider: parameters.NewProvider(s), client: client, cache: s.cache}
}

// FromConfig returns a provider with a Secrets Manager client built from
// cfg.
func FromConfig(cfg aws.Config) *Provider {
	return New(secretsmanager.NewFromConfig(cfg))
}

// readOptions are the Secrets Manager options of one read.
type readOptions struct {
	versionID    string
	versionStage string
	filters      []types.Filter
}

// WithVersionID reads the version of the secret with this id instead of
// the current one (VersionId). Versions are cached apart.
func WithVersionID(id string) parameters.Option {
	return parameters.WithStoreOption("version-id="+id, func(o *readOptions) { o.versionID = id })
}

// WithVersionStage reads the version of the secret with this staging label,
// AWSPREVIOUS for one, instead of AWSCURRENT (VersionStage). Stages are
// cached apart.
func WithVersionStage(stage string) parameters.Option {
	return parameters.WithStoreOption("version-stage="+stage, func(o *readOptions) { o.versionStage = stage })
}

// WithFilters narrows GetByName with Secrets Manager filters (by tag, by
// description), Python's Filters: the names asked for are then matched
// through a name filter, and only the secrets whose name is exactly one of
// them are returned.
func WithFilters(filters ...types.Filter) parameters.Option {
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		parts = append(parts, string(f.Key)+"="+strings.Join(f.Values, ","))
	}
	return parameters.WithStoreOption("filters="+strings.Join(parts, ";"), func(o *readOptions) { o.filters = filters })
}

// store reads Secrets Manager for the cache and transforms of
// parameters.Provider.
type store struct {
	client Client
	cache  *core.Cache
}

// Cache shares the provider's cache with GetByName.
func (s *store) Cache() *core.Cache { return s.cache }

// Get calls GetSecretValue and returns SecretString, or SecretBinary for a
// binary secret.
func (s *store) Get(ctx context.Context, name string, opts parameters.StoreOptions) ([]byte, error) {
	var o readOptions
	opts.Apply(&o)
	input := &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)}
	if o.versionID != "" {
		input.VersionId = aws.String(o.versionID)
	}
	if o.versionStage != "" {
		input.VersionStage = aws.String(o.versionStage)
	}
	out, err := s.client.GetSecretValue(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("secrets manager: %w", err)
	}
	if out.SecretString != nil {
		return []byte(*out.SecretString), nil
	}
	return out.SecretBinary, nil
}

// GetMultiple is not supported: Secrets Manager has no paths. Read several
// secrets with GetByName.
func (*store) GetMultiple(context.Context, string, parameters.StoreOptions) (map[string][]byte, error) {
	return nil, fmt.Errorf("secrets manager has no paths, read several secrets with GetByName: %w", errors.ErrUnsupported)
}

// defaults is the provider the shortcuts build on first use.
var defaults core.Defaults[struct{}, *Provider]

func init() {
	core.RegisterDefaults(defaults.Reset)
}

func defaultProvider(ctx context.Context) (*Provider, error) {
	return defaults.Get(struct{}{}, func() (*Provider, error) {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("load the default AWS configuration: %w", err)
		}
		return FromConfig(cfg), nil
	})
}

// GetSecret returns the secret name as a T (see parameters.Get), through a
// provider built on first use from the default AWS configuration: Python's
// get_secret.
func GetSecret[T any](ctx context.Context, name string, opts ...parameters.Option) (T, error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		var zero T
		return zero, &parameters.GetParameterError{Name: name, Err: err}
	}
	return parameters.Get[T](ctx, p, name, opts...)
}

// GetSecretsByName reads several secrets through the default provider:
// Python's get_secrets_by_name. See GetByName.
func GetSecretsByName[T any](ctx context.Context, names []string, opts ...parameters.Option) (values map[string]T, failed map[string]error, err error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		return nil, nil, &parameters.GetSecretError{Names: names, Err: err}
	}
	return GetByName[T](ctx, p, names, opts...)
}

// SetSecret writes a secret through the default provider: Python's
// set_secret. See Provider.Set.
func SetSecret(ctx context.Context, name string, value any, opts ...SetOption) (SetResult, error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		return SetResult{}, &parameters.SetSecretError{Name: name, Err: err}
	}
	return p.Set(ctx, name, value, opts...)
}
