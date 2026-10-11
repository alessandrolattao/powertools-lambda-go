package parameters

import (
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/parameters/internal/core"
)

// Transform names how a stored value is decoded before it is returned and
// cached.
type Transform string

// The transforms, as Python names them.
const (
	// TransformNone returns the value as stored.
	TransformNone Transform = core.TransformNone
	// TransformJSON parses the value as a JSON document, which Get and
	// GetMultiple then decode into the Go type the caller names.
	TransformJSON Transform = core.TransformJSON
	// TransformBinary decodes the value from base64. Characters outside the
	// base64 alphabet are skipped, as Python's base64.b64decode skips them.
	TransformBinary Transform = core.TransformBinary
	// TransformAuto picks the transform from the suffix of each value's name:
	// "config.json" is read as JSON, "key.binary" as base64, anything else as
	// stored.
	TransformAuto Transform = core.TransformAuto
)

// Option configures one read.
type Option func(*core.Options)

// WithMaxAge keeps the value cached for d, instead of
// POWERTOOLS_PARAMETERS_MAX_AGE (five minutes when unset). Zero or less reads
// the cache but never writes to it, as in Python.
func WithMaxAge(d time.Duration) Option {
	return func(o *core.Options) {
		o.MaxAge = d
		o.MaxAgeSet = true
	}
}

// WithForceFetch reads the value from its store even when the cache holds
// it; the fresh value is cached as usual.
func WithForceFetch(force bool) Option {
	return func(o *core.Options) { o.ForceFetch = force }
}

// WithTransform decodes the value with t. The value is cached after the
// transform, so the same name read with two transforms is fetched and
// cached twice, as in Python. ".json" and ".binary" are accepted as aliases
// of TransformJSON and TransformBinary.
func WithTransform(t Transform) Option {
	return func(o *core.Options) { o.Transform = string(t) }
}

// WithRaiseOnTransformError makes a read of several values fail on the first
// value whose transform fails. By default such a value is returned as the
// zero value of its type (Python returns None) and the others are returned
// as usual. A read of a single value always fails on a transform error.
func WithRaiseOnTransformError(raise bool) Option {
	return func(o *core.Options) { o.RaiseOnTransformError = raise }
}

// WithStoreOption carries an option of a specific store along with the
// common ones: the store reads it with StoreOptions.Apply. apply receives the
// store's own options type O.
//
// variant says how the option changes the value read, for example
// "decrypt=true", or "" when it does not change it (a page size, a
// consistency level). Values read with different variants are cached apart.
func WithStoreOption[O any](variant string, apply func(*O)) Option {
	return func(o *core.Options) {
		o.Store = append(o.Store, core.StoreOption{Variant: variant, Apply: apply})
	}
}

// StoreOptions are the store-specific options of one read, handed to the
// Store.
type StoreOptions struct {
	list []core.StoreOption
}

// Apply applies to target, in the order the caller gave them, the options
// written for its type with WithStoreOption. Set the defaults in target
// first; options for other stores are skipped.
func (s StoreOptions) Apply[O any](target *O) {
	core.ApplyStore(s.list, target)
}

// collect applies opts and checks the transform.
func collect(name string, opts []Option) (core.Options, error) {
	var o core.Options
	for _, opt := range opts {
		opt(&o)
	}
	normalized, err := core.NormalizeTransform(o.Transform)
	if err != nil {
		return o, &TransformError{Name: name, Transform: Transform(o.Transform), Err: err}
	}
	o.Transform = normalized
	return o, nil
}
