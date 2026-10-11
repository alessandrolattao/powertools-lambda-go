package parameters

import (
	"context"
	"errors"

	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// Store reads values from a backend: the part of a provider that talks to
// it, Python's BaseProvider._get and _get_multiple. NewProvider puts the
// cache and the transforms in front of it, so a custom provider (Vault, S3,
// a database) only implements these two methods.
//
// A store may be called concurrently. It returns the value as stored; the
// provider wraps its errors in GetParameterError.
type Store interface {
	// Get returns the value of name.
	Get(ctx context.Context, name string, opts StoreOptions) ([]byte, error)
	// GetMultiple returns the values under path, keyed by their name
	// relative to it. A store without paths returns an error matching
	// errors.ErrUnsupported.
	GetMultiple(ctx context.Context, path string, opts StoreOptions) (map[string][]byte, error)
}

// Provider reads values through a Store, caching them and applying the
// transforms. It is safe for concurrent use: create it once, in main or at
// package level, and share it across invocations.
type Provider struct {
	store Store
	cache *core.Cache
}

// NewProvider returns a provider that reads through store.
func NewProvider(store Store) *Provider {
	cache := core.NewCache()
	if holder, ok := store.(core.CacheHolder); ok {
		cache = holder.Cache()
	}
	return &Provider{store: store, cache: cache}
}

// Source is what Get and GetMultiple read from: a *Provider, or a provider
// of the packages below this one, which embeds it.
type Source interface {
	parametersProvider() *Provider
}

func (p *Provider) parametersProvider() *Provider { return p }

// Get returns the value of name as a string: Get[string].
func (p *Provider) Get(ctx context.Context, name string, opts ...Option) (string, error) {
	return Get[string](ctx, p, name, opts...)
}

// GetMultiple returns the values under path as strings: GetMultiple[string].
func (p *Provider) GetMultiple(ctx context.Context, path string, opts ...Option) (map[string]string, error) {
	return GetMultiple[string](ctx, p, path, opts...)
}

// ClearCache forgets every value the provider cached.
func (p *Provider) ClearCache() {
	p.cache.Clear()
}

// Get reads the value of name from src and returns it as a T.
//
// Without a transform the value is returned as stored: T is a string, a
// []byte, or any (which then holds a string). With TransformBinary it is
// decoded from base64 first, and any holds a []byte. With TransformJSON it
// is decoded into T, whatever T is: a struct, a map, a slice, a number.
// TransformAuto picks one of those from the name's suffix.
//
// The value comes from the cache when it is there and fresh, unless
// WithForceFetch says otherwise. Errors are a *GetParameterError when the
// store fails and a *TransformError when the value cannot become a T.
func Get[T any](ctx context.Context, src Source, name string, opts ...Option) (T, error) {
	var zero T
	p := src.parametersProvider()
	o, err := collect(name, opts)
	if err != nil {
		return zero, err
	}
	value, err := core.Fetch(ctx, p.cache, core.SingleKey(name, &o), o.ResolvedMaxAge(), o.ForceFetch, func(ctx context.Context) (core.Value, error) {
		raw, fetchErr := p.store.Get(ctx, name, StoreOptions{list: o.Store})
		if fetchErr != nil {
			return core.Value{}, fetchErr
		}
		value, transformErr := core.Apply(name, raw, o.Transform)
		if transformErr != nil {
			return core.Value{}, &TransformError{Name: name, Transform: Transform(o.Transform), Err: transformErr}
		}
		return value, nil
	})
	if err != nil {
		return zero, wrapGet(name, err)
	}
	out, err := core.Convert[T](value)
	if err != nil {
		return zero, &TransformError{Name: name, Transform: Transform(o.Transform), Err: err}
	}
	return out, nil
}

// GetMultiple reads the values under path from src and returns them as Ts,
// keyed by their name relative to path. Each value becomes a T as in Get;
// with TransformAuto each one by its own suffix, so a GetMultiple[any] may
// mix strings, bytes and decoded JSON.
//
// A value whose transform fails is the zero T, unless
// WithRaiseOnTransformError makes the whole read fail. Not every store has
// paths: AppConfig and Secrets Manager do not.
func GetMultiple[T any](ctx context.Context, src Source, path string, opts ...Option) (map[string]T, error) {
	p := src.parametersProvider()
	o, err := collect(path, opts)
	if err != nil {
		return nil, err
	}
	values, err := core.Fetch(ctx, p.cache, core.MultipleKey(path, &o), o.ResolvedMaxAge(), o.ForceFetch, func(ctx context.Context) (map[string]core.Value, error) {
		raw, fetchErr := p.store.GetMultiple(ctx, path, StoreOptions{list: o.Store})
		if fetchErr != nil {
			return nil, fetchErr
		}
		return transformAll(raw, &o)
	})
	if err != nil {
		return nil, wrapGet(path, err)
	}
	return convertAll[T](values, &o)
}

// transformAll applies the transform to every value of a multiple read.
func transformAll(raw map[string][]byte, o *core.Options) (map[string]core.Value, error) {
	values := make(map[string]core.Value, len(raw))
	for name, data := range raw {
		value, err := core.Apply(name, data, o.Transform)
		if err != nil {
			if o.RaiseOnTransformError {
				return nil, &TransformError{Name: name, Transform: Transform(o.Transform), Err: err}
			}
			value = core.Value{Kind: core.KindFailed, Err: err}
		}
		values[name] = value
	}
	return values, nil
}

// convertAll turns transformed values into Ts. A value whose transform
// failed fails the read when o says so, even when it comes from the cache,
// where an earlier read that tolerated the failure left it.
func convertAll[T any](values map[string]core.Value, o *core.Options) (map[string]T, error) {
	out := make(map[string]T, len(values))
	for name, value := range values {
		if value.Kind == core.KindFailed && o.RaiseOnTransformError {
			return nil, &TransformError{Name: name, Transform: Transform(o.Transform), Err: value.Err}
		}
		converted, err := core.Convert[T](value)
		if err != nil {
			return nil, &TransformError{Name: name, Transform: Transform(o.Transform), Err: err}
		}
		out[name] = converted
	}
	return out, nil
}

// wrapGet makes err a *GetParameterError, unless it already is one of this
// package's errors.
func wrapGet(name string, err error) error {
	if errors.Is(err, ErrTransform) || errors.Is(err, ErrGetParameter) {
		return err
	}
	return &GetParameterError{Name: name, Err: err}
}

// ClearCaches forgets the default providers of the package-level shortcuts
// (ssm.GetParameter, secrets.GetSecret, appconfig.GetAppConfig and the
// others), with everything they cached: the next shortcut builds a new
// provider and a new SDK client. Providers you created yourself are not
// affected; call their ClearCache.
func ClearCaches() {
	core.ResetDefaults()
}
