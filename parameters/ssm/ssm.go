package ssm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// Client is the part of the SSM client the provider calls; *ssm.Client
// satisfies it.
type Client interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	GetParameters(ctx context.Context, params *ssm.GetParametersInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersOutput, error)
	GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
	PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
}

// Provider reads and writes SSM Parameter Store parameters. It is safe for
// concurrent use: create it once and share it across invocations.
type Provider struct {
	*parameters.Provider

	client Client
	cache  *core.Cache
}

// New returns a provider that calls SSM through client.
func New(client Client) *Provider {
	s := &store{client: client, cache: core.NewCache()}
	return &Provider{Provider: parameters.NewProvider(s), client: client, cache: s.cache}
}

// FromConfig returns a provider with an SSM client built from cfg.
func FromConfig(cfg aws.Config) *Provider {
	return New(ssm.NewFromConfig(cfg))
}

// readOptions are the SSM options of one read.
type readOptions struct {
	decrypt      *bool
	recursive    bool
	filters      []types.ParameterStringFilter
	raiseOnError *bool
}

// WithDecrypt decrypts SecureString parameters (WithDecryption), instead of
// POWERTOOLS_PARAMETERS_SSM_DECRYPT (false when unset). Values read with and
// without decryption are cached apart.
func WithDecrypt(decrypt bool) parameters.Option {
	return parameters.WithStoreOption("decrypt="+strconv.FormatBool(decrypt), func(o *readOptions) { o.decrypt = &decrypt })
}

// WithRecursive makes a read by path include the parameters of every level
// below it, not only the direct children. Python's defaults are kept:
// Provider.GetMultiple is not recursive unless asked, the GetParameters
// shortcut is.
func WithRecursive(recursive bool) parameters.Option {
	return parameters.WithStoreOption("recursive="+strconv.FormatBool(recursive), func(o *readOptions) { o.recursive = recursive })
}

// WithParameterFilters narrows a read by path with SSM parameter filters
// (ParameterFilters of GetParametersByPath), for example by type or label.
func WithParameterFilters(filters ...types.ParameterStringFilter) parameters.Option {
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		parts = append(parts, aws.ToString(f.Key)+aws.ToString(f.Option)+strings.Join(f.Values, ","))
	}
	return parameters.WithStoreOption("filters="+strings.Join(parts, ";"), func(o *readOptions) { o.filters = filters })
}

// WithRaiseOnError decides how GetByName treats a parameter it cannot read:
// true (the default, as in Python) fails the whole read; false returns the
// other values and the failures apart, Python's raise_on_error=False.
func WithRaiseOnError(raise bool) parameters.Option {
	return parameters.WithStoreOption("", func(o *readOptions) { o.raiseOnError = &raise })
}

// resolveDecrypt returns the explicit choice, else
// POWERTOOLS_PARAMETERS_SSM_DECRYPT, read on every call as in Python.
func resolveDecrypt(explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	return env.Bool(env.ParametersSSMDecrypt, false)
}

// store reads SSM for the cache and transforms of parameters.Provider.
type store struct {
	client Client
	cache  *core.Cache
}

// Cache shares the provider's cache with GetByName.
func (s *store) Cache() *core.Cache { return s.cache }

// Get calls GetParameter.
func (s *store) Get(ctx context.Context, name string, opts parameters.StoreOptions) ([]byte, error) {
	var o readOptions
	opts.Apply(&o)
	out, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(resolveDecrypt(o.decrypt)),
	})
	if err != nil {
		return nil, fmt.Errorf("ssm: %w", err)
	}
	if out.Parameter == nil {
		return nil, parameters.ErrNotFound
	}
	return []byte(aws.ToString(out.Parameter.Value)), nil
}

// GetMultiple calls GetParametersByPath, every page of it. The names are
// returned relative to path, without leading slashes: "/dev/webhook/config"
// under "/dev" is "webhook/config".
func (s *store) GetMultiple(ctx context.Context, path string, opts parameters.StoreOptions) (map[string][]byte, error) {
	var o readOptions
	opts.Apply(&o)
	pages := ssm.NewGetParametersByPathPaginator(s.client, &ssm.GetParametersByPathInput{
		Path:             aws.String(path),
		WithDecryption:   aws.Bool(resolveDecrypt(o.decrypt)),
		Recursive:        aws.Bool(o.recursive),
		ParameterFilters: o.filters,
	})
	values := make(map[string][]byte)
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ssm: %w", err)
		}
		for _, p := range page.Parameters {
			name := strings.TrimLeft(strings.TrimPrefix(aws.ToString(p.Name), path), "/")
			values[name] = []byte(aws.ToString(p.Value))
		}
	}
	return values, nil
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

// GetParameter returns the parameter name as a T (see parameters.Get),
// through a provider built on first use from the default AWS configuration:
// Python's get_parameter.
func GetParameter[T any](ctx context.Context, name string, opts ...parameters.Option) (T, error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		var zero T
		return zero, &parameters.GetParameterError{Name: name, Err: err}
	}
	return parameters.Get[T](ctx, p, name, opts...)
}

// GetParameters returns the parameters under path as Ts (see
// parameters.GetMultiple), through the default provider: Python's
// get_parameters. Unlike Provider.GetMultiple it is recursive unless
// WithRecursive(false) says otherwise, as in Python.
func GetParameters[T any](ctx context.Context, path string, opts ...parameters.Option) (map[string]T, error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		return nil, &parameters.GetParameterError{Name: path, Err: err}
	}
	return parameters.GetMultiple[T](ctx, p, path, append([]parameters.Option{WithRecursive(true)}, opts...)...)
}

// GetParametersByName reads several parameters by name through the default
// provider: Python's get_parameters_by_name. See GetByName.
func GetParametersByName[T any](ctx context.Context, params map[string][]parameters.Option, opts ...parameters.Option) (values map[string]T, failed map[string]error, err error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		return nil, nil, &parameters.GetParameterError{Name: strings.Join(names(params), ","), Err: err}
	}
	return GetByName[T](ctx, p, params, opts...)
}

// SetParameter writes a parameter through the default provider: Python's
// set_parameter. See Provider.Set.
func SetParameter[V string | []string](ctx context.Context, name string, value V, opts ...SetOption) (SetResult, error) {
	p, err := defaultProvider(ctx)
	if err != nil {
		return SetResult{}, &parameters.SetParameterError{Name: name, Err: err}
	}
	return p.Set(ctx, name, value, opts...)
}
