package ssm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// maxGetParameters is how many names one GetParameters call accepts.
const maxGetParameters = 10

// GetByName returns the string value of several parameters: GetByName[string].
func (p *Provider) GetByName(ctx context.Context, params map[string][]parameters.Option, opts ...parameters.Option) (values map[string]string, failed map[string]error, err error) {
	return GetByName[string](ctx, p, params, opts...)
}

// GetByName reads several parameters by their full name and returns them
// as Ts (see parameters.Get): Python's get_parameters_by_name.
//
// params maps each name to its own options, which override opts for that
// parameter: parameters.WithTransform, WithDecrypt, parameters.WithMaxAge.
// As in Python, a parameter without its own transform takes the one of
// opts. Names may carry a version or label selector ("/app/key:3") or be
// ARNs.
//
// The parameters that need no decryption are read with GetParameters, ten
// names per call; those that need it are read one by one with GetParameter,
// unless every parameter needs it, in which case they are all read with
// GetParameters and decryption, Python's strategy. Every value is cached like
// a value read with Get under the same options, and found there next time.
//
// A parameter that cannot be read (missing, undecryptable, failing its
// transform) fails the whole read, unless WithRaiseOnError(false) is given:
// then the values read are returned along with the failures, by name.
// Errors that concern the call rather than one parameter (throttling, access
// denied) always fail the read.
func GetByName[T any](ctx context.Context, p *Provider, params map[string][]parameters.Option, opts ...parameters.Option) (values map[string]T, failed map[string]error, err error) {
	r, err := newByName[T](p, params, opts)
	if err != nil {
		return nil, nil, err
	}
	var batch, decrypt []item
	for _, it := range r.items {
		if it.decrypt {
			decrypt = append(decrypt, it)
		} else {
			batch = append(batch, it)
		}
	}
	if len(decrypt) == len(r.items) {
		err = r.batch(ctx, decrypt, true)
	} else {
		if err = r.oneByOne(ctx, decrypt); err == nil {
			err = r.batch(ctx, batch, false)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if len(r.failed) == 0 {
		return r.values, nil, nil
	}
	return r.values, r.failed, nil
}

// item is one parameter of a GetByName and its resolved options.
type item struct {
	name    string
	opts    []parameters.Option
	options core.Options
	decrypt bool
}

// byName is the state of one GetByName.
type byName[T any] struct {
	p      *Provider
	items  []item
	raise  bool
	force  bool
	values map[string]T
	failed map[string]error
}

func newByName[T any](p *Provider, params map[string][]parameters.Option, opts []parameters.Option) (*byName[T], error) {
	global, err := apply("", opts)
	if err != nil {
		return nil, err
	}
	var ro readOptions
	core.ApplyStore(global.Store, &ro)
	r := &byName[T]{
		p:      p,
		raise:  ro.raiseOnError == nil || *ro.raiseOnError,
		force:  global.ForceFetch,
		values: make(map[string]T, len(params)),
		failed: make(map[string]error),
	}
	// Go maps have no order: go by name, so that batches and failures are
	// the same from one run to the next.
	for _, name := range names(params) {
		own := params[name]
		merged := slices.Concat(opts, own)
		options, err := apply(name, merged)
		if err != nil {
			return nil, err
		}
		if options.Transform == core.TransformNone {
			// As in Python, a parameter's own empty transform falls back to
			// the shared one.
			options.Transform = global.Transform
			merged = append(merged, parameters.WithTransform(parameters.Transform(global.Transform)))
		}
		var pro readOptions
		core.ApplyStore(options.Store, &pro)
		r.items = append(r.items, item{name: name, opts: merged, options: options, decrypt: resolveDecrypt(pro.decrypt)})
	}
	return r, nil
}

// apply resolves opts the way parameters.Get does.
func apply(name string, opts []parameters.Option) (core.Options, error) {
	var o core.Options
	for _, opt := range opts {
		opt(&o)
	}
	normalized, err := core.NormalizeTransform(o.Transform)
	if err != nil {
		return o, &parameters.TransformError{Name: name, Transform: parameters.Transform(o.Transform), Err: err}
	}
	o.Transform = normalized
	return o, nil
}

func names(params map[string][]parameters.Option) []string {
	return slices.Sorted(maps.Keys(params))
}

// fail records that name could not be read, or returns err when the read
// must stop at the first failure.
func (r *byName[T]) fail(name string, err error) error {
	if r.raise {
		return err
	}
	r.failed[name] = err
	return nil
}

// oneByOne reads the parameters that need decryption one GetParameter at a
// time, through the provider's own Get: Python does the same, so that one
// key that cannot be used does not fail a whole batch.
func (r *byName[T]) oneByOne(ctx context.Context, items []item) error {
	for _, it := range items {
		value, err := parameters.Get[T](ctx, r.p, it.name, it.opts...)
		if err != nil {
			if failErr := r.fail(it.name, err); failErr != nil {
				return failErr
			}
			continue
		}
		r.values[it.name] = value
	}
	return nil
}

// batch reads items with GetParameters, ten at a time, after taking what
// the cache already holds.
func (r *byName[T]) batch(ctx context.Context, items []item, decrypt bool) error {
	var missing []item
	for _, it := range items {
		if !r.force {
			if value, ok := core.Lookup[core.Value](r.p.cache, core.SingleKey(it.name, &it.options)); ok {
				if err := r.keep(it, value); err != nil {
					return err
				}
				continue
			}
		}
		missing = append(missing, it)
	}
	for chunk := range slices.Chunk(missing, maxGetParameters) {
		if err := r.chunk(ctx, chunk, decrypt); err != nil {
			return err
		}
	}
	return nil
}

// chunk reads up to ten parameters in one GetParameters call.
func (r *byName[T]) chunk(ctx context.Context, items []item, decrypt bool) error {
	requested := make([]string, 0, len(items))
	for _, it := range items {
		requested = append(requested, it.name)
	}
	out, err := r.p.client.GetParameters(ctx, &ssm.GetParametersInput{Names: requested, WithDecryption: aws.Bool(decrypt)})
	if err != nil {
		return r.callFailed(items, requested, err)
	}
	found := match(items, out.Parameters)
	// A name SSM lists in InvalidParameters, or (Python: a KeyError) answers
	// under another name than the one asked for, was not found. As in Python,
	// when that fails the read the error names all of them.
	var notFound []error
	for _, it := range items {
		if _, ok := found[it.name]; !ok {
			notFound = append(notFound, &parameters.GetParameterError{Name: it.name, Err: parameters.ErrNotFound})
		}
	}
	if r.raise && len(notFound) > 0 {
		return errors.Join(notFound...)
	}
	for _, it := range items {
		param, ok := found[it.name]
		if !ok {
			r.failed[it.name] = &parameters.GetParameterError{Name: it.name, Err: parameters.ErrNotFound}
			continue
		}
		if err := r.store(it, aws.ToString(param.Value)); err != nil {
			return err
		}
	}
	return nil
}

// callFailed handles a GetParameters call that failed as a whole. A key that
// cannot be used, or an SSM failure, fails each parameter of the call
// (Python drops them silently when raise_on_error is on, which is fixed
// here); any other error (throttling, access denied) fails the read.
func (r *byName[T]) callFailed(items []item, requested []string, err error) error {
	if !errors.As(err, new(*types.InvalidKeyId)) && !errors.As(err, new(*types.InternalServerError)) {
		return &parameters.GetParameterError{Name: fmt.Sprint(requested), Err: fmt.Errorf("ssm: %w", err)}
	}
	for _, it := range items {
		if failErr := r.fail(it.name, &parameters.GetParameterError{Name: it.name, Err: fmt.Errorf("ssm: %w", err)}); failErr != nil {
			return failErr
		}
	}
	return nil
}

// store transforms and caches the value SSM returned for it, then keeps it.
// A value whose transform fails is a failure of that parameter, never
// cached (Python caches None for it).
func (r *byName[T]) store(it item, raw string) error {
	value, err := core.Apply(it.name, []byte(raw), it.options.Transform)
	if err != nil {
		return r.fail(it.name, &parameters.TransformError{Name: it.name, Transform: parameters.Transform(it.options.Transform), Err: err})
	}
	r.p.cache.Store(core.SingleKey(it.name, &it.options), value, it.options.ResolvedMaxAge())
	return r.keep(it, value)
}

// keep converts a transformed value into a T and keeps it.
func (r *byName[T]) keep(it item, value core.Value) error {
	converted, err := core.Convert[T](value)
	if err != nil {
		return r.fail(it.name, &parameters.TransformError{Name: it.name, Transform: parameters.Transform(it.options.Transform), Err: err})
	}
	r.values[it.name] = converted
	return nil
}

// match pairs the parameters GetParameters returned with the names asked
// for. SSM answers a name with a selector ("/app/key:3") or an ARN under
// its plain name, so each parameter is looked up under every form it can
// have been asked by.
func match(items []item, returned []types.Parameter) map[string]types.Parameter {
	asked := make(map[string]bool, len(items))
	for _, it := range items {
		asked[it.name] = true
	}
	found := make(map[string]types.Parameter, len(returned))
	for _, param := range returned {
		name, arn, selector := aws.ToString(param.Name), aws.ToString(param.ARN), aws.ToString(param.Selector)
		for _, candidate := range []string{name, name + selector, arn, arn + selector} {
			if candidate != "" && asked[candidate] {
				found[candidate] = param
			}
		}
	}
	return found
}
