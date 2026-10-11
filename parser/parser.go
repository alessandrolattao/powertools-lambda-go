package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
)

// ErrParse is what every parsing failure matches with errors.Is.
var ErrParse = errors.New("parse error")

// ParseError reports data that could not be decoded into the model, or that
// the model's Validate method rejected.
type ParseError struct {
	// Model is the Go type the data was parsed into.
	Model string
	// Source says where the payload came from in the event, such as
	// "Records[2].body"; it is empty when the whole data was parsed.
	Source string
	// Err is the cause: a *json.SemanticError, a *jsontext.SyntacticError,
	// an envelope error, or the error Validate returned.
	Err error
}

func (e *ParseError) Error() string {
	if e.Source == "" {
		return fmt.Sprintf("parse %s: %v", e.Model, e.Err)
	}
	return fmt.Sprintf("parse %s from %s: %v", e.Model, e.Source, e.Err)
}

// Unwrap returns the cause.
func (e *ParseError) Unwrap() error { return e.Err }

// Is makes every ParseError match ErrParse.
func (*ParseError) Is(target error) bool { return target == ErrParse }

// Validator is implemented by models that check themselves after decoding,
// the Go counterpart of Pydantic's field and model validators. Parse calls
// Validate on the decoded value, and on each element when the model is a
// slice or an array.
type Validator interface {
	Validate() error
}

// Option configures parsing.
type Option func(*config)

type config struct {
	envelope Envelope
	json     []json.Options
}

func newConfig(opts []Option) config {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithEnvelope unwraps the payload of an event source before parsing:
// the model then describes the payload (or, for sources that batch records,
// a slice of payloads), not the event.
func WithEnvelope(envelope Envelope) Option {
	return func(c *config) { c.envelope = envelope }
}

// WithJSONOptions passes options to encoding/json/v2 when decoding the
// model, for example json.RejectUnknownMembers(true) for Pydantic's
// extra="forbid", or json.MatchCaseInsensitiveNames(true).
func WithJSONOptions(opts ...json.Options) Option {
	return func(c *config) { c.json = append(c.json, opts...) }
}

// Parse decodes data into a T and validates it. Data is raw JSON
// ([]byte, jsontext.Value, or a string holding JSON text, as Powertools
// Python parses a str) or any other value, which is marshalled to JSON first
// (a map, an aws-lambda-go event).
//
// With WithEnvelope, the envelope unwraps the payload first; a missing
// payload (a GET request without body) gives the zero T, not validated, as
// Python gives None.
//
// Decoding follows encoding/json/v2 defaults, as lenient as Pydantic's: unknown
// members are ignored, missing ones stay zero. Use Validate for required
// members and value constraints.
func Parse[T any](data any, opts ...Option) (T, error) {
	var out T
	cfg := newConfig(opts)
	raw, err := rawJSON(data)
	if err != nil {
		return out, parseError[T]("", err)
	}
	if cfg.envelope != nil {
		if raw, err = cfg.envelope.Unwrap(raw); err != nil {
			return out, parseError[T]("", err)
		}
	}
	return decode[T](raw, "", cfg.json)
}

// rawJSON returns data as JSON text.
func rawJSON(data any) (jsontext.Value, error) {
	switch v := data.(type) {
	case jsontext.Value:
		return v, nil
	case []byte:
		return v, nil
	case string:
		return jsontext.Value(v), nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal %T: %w", data, err)
		}
		return raw, nil
	}
}

// decode decodes raw into a T and validates it. A JSON null, or nothing at
// all, is a missing payload: the zero T, not validated. On failure it
// returns the zero T, never a half-decoded one.
func decode[T any](raw []byte, source string, opts []json.Options) (T, error) {
	var out T
	if isNull(raw) {
		return out, nil
	}
	var zero T
	if err := json.Unmarshal(raw, &out, opts...); err != nil {
		return zero, parseError[T](source, err)
	}
	if err := validate(&out); err != nil {
		return zero, parseError[T](source, err)
	}
	return out, nil
}

func isNull(raw []byte) bool {
	value := jsontext.Value(raw)
	return len(raw) == 0 || value.IsValid() && value.Kind() == 'n'
}

func parseError[T any](source string, err error) *ParseError {
	return &ParseError{Model: reflect.TypeFor[T]().String(), Source: source, Err: err}
}

// validate calls Validate on the value ptr points to, or on each of its
// elements when it is a slice or an array.
func validate(ptr any) error {
	rv := reflect.ValueOf(ptr).Elem()
	if v, ok := validatorOf(rv); ok {
		return v.Validate()
	}
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil
	}
	for i := range rv.Len() {
		v, ok := validatorOf(rv.Index(i))
		if !ok {
			continue
		}
		if err := v.Validate(); err != nil {
			return fmt.Errorf("element %d: %w", i, err)
		}
	}
	return nil
}

// validatorOf returns the Validator rv is, through a pointer to it when the
// method has a pointer receiver. A nil pointer is a missing payload: nothing
// to validate.
func validatorOf(rv reflect.Value) (Validator, bool) {
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil, false
	}
	if rv.CanAddr() {
		if v, ok := reflect.TypeAssert[Validator](rv.Addr()); ok {
			return v, true
		}
	}
	if rv.CanInterface() {
		return reflect.TypeAssert[Validator](rv)
	}
	return nil, false
}
