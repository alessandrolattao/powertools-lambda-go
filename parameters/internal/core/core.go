// Package core is the machinery the parameters packages share: the options
// of one read, the transforms, the conversion of a value into the Go type the
// caller names, the cache and the registry of default providers. It is
// internal so that every provider under parameters/ uses it without it
// becoming part of the public API.
package core

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// DefaultMaxAge is how long a value stays cached when neither the read nor
// POWERTOOLS_PARAMETERS_MAX_AGE says otherwise, as in Powertools Python.
const DefaultMaxAge = 5 * time.Minute

// The transforms, by their Python names.
const (
	TransformNone   = ""
	TransformJSON   = "json"
	TransformBinary = "binary"
	TransformAuto   = "auto"
)

// Errors of the transforms and of the conversion into a Go type.
var (
	// ErrUnknownTransform reports a transform that is none of the above.
	ErrUnknownTransform = errors.New("unknown transform")
	// ErrInvalidJSON reports a value that is not a JSON document.
	ErrInvalidJSON = errors.New("value is not valid JSON")
	// ErrIncorrectPadding reports base64 data whose padding is missing.
	ErrIncorrectPadding = errors.New("incorrect base64 padding")
	// ErrUnsupportedType reports a Go type that cannot hold a value read
	// without the JSON transform.
	ErrUnsupportedType = errors.New("type cannot hold a text value, read it with the JSON transform")
)

// StoreOption is an option of one specific store, carried along with the
// common ones. Apply is a func(*O) for the store's option type O; Variant
// says how the option changes the value read ("" when it does not), so that
// the cache keeps values read with different options apart.
type StoreOption struct {
	Variant string
	Apply   any
}

// Options are the options of one read, once applied.
type Options struct {
	MaxAge                time.Duration
	MaxAgeSet             bool
	ForceFetch            bool
	Transform             string
	RaiseOnTransformError bool
	Store                 []StoreOption
}

// ResolvedMaxAge returns the explicit max age, else
// POWERTOOLS_PARAMETERS_MAX_AGE, else DefaultMaxAge. The variable is read on
// every call, as Python does, so that changing it at runtime takes effect.
func (o *Options) ResolvedMaxAge() time.Duration {
	if o.MaxAgeSet {
		return o.MaxAge
	}
	return env.Seconds(env.ParametersMaxAge, DefaultMaxAge)
}

// Variant joins the variants of the store options, in the order they were
// given.
func (o *Options) Variant() string {
	parts := make([]string, 0, len(o.Store))
	for _, s := range o.Store {
		if s.Variant != "" {
			parts = append(parts, s.Variant)
		}
	}
	return strings.Join(parts, "|")
}

// ApplyStore applies to target, in order, every store option written for its
// type.
func ApplyStore[O any](options []StoreOption, target *O) {
	for _, s := range options {
		if apply, ok := s.Apply.(func(*O)); ok {
			apply(target)
		}
	}
}

// NormalizeTransform returns the canonical name of transform t, accepting the
// ".json" and ".binary" aliases Python accepts.
func NormalizeTransform(t string) (string, error) {
	switch t {
	case TransformNone, TransformJSON, TransformBinary, TransformAuto:
		return t, nil
	case ".json":
		return TransformJSON, nil
	case ".binary":
		return TransformBinary, nil
	default:
		return "", fmt.Errorf("%w %q", ErrUnknownTransform, t)
	}
}

// ResolveTransform returns the transform that applies to the value named
// name: transform itself, or for TransformAuto the one the suffix after the
// last dot names (".json" or ".binary"), none for any other suffix. As in
// Python, a name without a dot is its own suffix.
func ResolveTransform(name, transform string) string {
	if transform != TransformAuto {
		return transform
	}
	suffix := name[strings.LastIndexByte(name, '.')+1:]
	switch suffix {
	case TransformJSON, TransformBinary:
		return suffix
	default:
		return TransformNone
	}
}

// Kind says what a Value holds.
type Kind uint8

// The kinds of Value.
const (
	// KindText is a value as the store returned it.
	KindText Kind = iota
	// KindBinary is the bytes a binary transform decoded.
	KindBinary
	// KindJSON is a JSON document, validated by the JSON transform.
	KindJSON
	// KindFailed is a value whose transform failed in a read of several
	// values that tolerates it: Python gives None, Go the zero value.
	KindFailed
)

// Value is a value after its transform: what the cache keeps, and what is
// converted into the caller's type on every read, so that callers never
// share mutable state through the cache.
type Value struct {
	Data []byte
	Kind Kind
	// Err is why the transform of a KindFailed value failed.
	Err error
}

// jsonOptions read JSON as Python's json.loads does: a repeated object
// member is not an error, the last one wins.
var jsonOptions = json.JoinOptions(jsontext.AllowDuplicateNames(true))

// Apply transforms raw, the value named name, with transform (already
// normalized).
func Apply(name string, raw []byte, transform string) (Value, error) {
	switch ResolveTransform(name, transform) {
	case TransformJSON:
		if err := json.Unmarshal(raw, new(jsontext.Value), jsonOptions); err != nil {
			return Value{}, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
		}
		return Value{Data: raw, Kind: KindJSON}, nil
	case TransformBinary:
		decoded, err := DecodeBase64(raw)
		if err != nil {
			return Value{}, err
		}
		return Value{Data: decoded, Kind: KindBinary}, nil
	default:
		return Value{Data: raw, Kind: KindText}, nil
	}
}

// DecodeBase64 decodes standard base64 the way Python's base64.b64decode
// does without validation: characters outside the alphabet, whitespace
// included, are discarded, padding is required, and anything after the
// padding is ignored.
func DecodeBase64(raw []byte) ([]byte, error) {
	data := make([]byte, 0, len(raw))
	padding := 0
	for _, c := range raw {
		switch {
		case c == '=':
			padding++
		case padding > 0:
			// Python stops decoding at the padding; what follows is not part
			// of this value.
		case isBase64(c):
			data = append(data, c)
		}
	}
	if len(data)%4 == 1 {
		return nil, fmt.Errorf("%w: %d data characters cannot be 1 more than a multiple of 4", ErrIncorrectPadding, len(data))
	}
	if needed := (4 - len(data)%4) % 4; padding < needed {
		return nil, ErrIncorrectPadding
	}
	decoded := make([]byte, base64.RawStdEncoding.DecodedLen(len(data)))
	n, err := base64.RawStdEncoding.Decode(decoded, data)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return decoded[:n], nil
}

func isBase64(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
}

// Convert turns v into a T. A JSON value is decoded into T; a text or binary
// value goes into a string, a []byte or an empty interface (a string for
// text, a []byte for binary, as Python returns str and bytes); a failed one
// gives the zero T.
func Convert[T any](v Value) (T, error) {
	var out T
	switch v.Kind {
	case KindFailed:
		return out, nil
	case KindJSON:
		if err := json.Unmarshal(v.Data, &out, jsonOptions); err != nil {
			return out, fmt.Errorf("decode JSON into %T: %w", out, err)
		}
		return out, nil
	default:
		target := reflect.ValueOf(&out).Elem()
		switch {
		case target.Kind() == reflect.String:
			target.SetString(string(v.Data))
		case target.Kind() == reflect.Slice && target.Type().Elem().Kind() == reflect.Uint8:
			target.SetBytes(bytes.Clone(v.Data))
		case target.Kind() == reflect.Interface && target.NumMethod() == 0:
			if v.Kind == KindBinary {
				target.Set(reflect.ValueOf(bytes.Clone(v.Data)))
			} else {
				target.Set(reflect.ValueOf(string(v.Data)))
			}
		default:
			return out, fmt.Errorf("%w: %T", ErrUnsupportedType, out)
		}
		return out, nil
	}
}
