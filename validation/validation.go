package validation

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// rootURL is where a compiled schema lives for reference resolution. A
// schema with an "$id" is also known by it, which is the base its relative
// references resolve against.
const rootURL = "mem:///schema.json"

// Schema is a compiled JSON Schema. It is safe for concurrent use: compile
// it once, at package level or in init, and validate every invocation
// against it.
type Schema struct {
	compiled *jsonschema.Schema
	// docs holds the JSON documents the schema was compiled from, by URL,
	// to report the definition a value breaks.
	docs map[string]any
}

// schemas caches what Validate compiles, by source and options, so that a
// schema given as a document is compiled once rather than on every call.
var schemas = newCache[*Schema](128)

// envelopes caches the compiled envelope expressions.
var envelopes = newCache[*jmespath.Expression](64)

// Compile compiles a JSON Schema. The schema is raw JSON ([]byte,
// jsontext.Value or string) or a value that marshals to one, such as a
// map[string]any or a bool. A failure is an *InvalidSchemaFormatError.
func Compile(schema any, opts ...Option) (*Schema, error) {
	o := newOptions(opts)
	raw, err := sourceJSON(schema)
	if err != nil {
		return nil, &InvalidSchemaFormatError{Formats: o.formatNames(), Err: err}
	}
	return compile(context.Background(), raw, &o)
}

// MustCompile is Compile for schemas known to be valid; it panics
// otherwise.
func MustCompile(schema any, opts ...Option) *Schema {
	s, err := Compile(schema, opts...)
	if err != nil {
		panic(err)
	}
	return s
}

// Validate checks event against schema and returns the validated data:
// Powertools Python's validate.
//
// The event is raw JSON ([]byte, jsontext.Value) or a value that marshals to
// JSON (an aws-lambda-go event, a map[string]any); a Go string is a JSON
// string. The schema is a *Schema from Compile, or a schema document, which
// is compiled on first use and cached. WithEnvelope selects the part of the
// event to validate.
//
// The returned data is the selected part of the event as a decoded JSON
// value, with the defaults of the schema filled in (see WithDefaults).
// Numbers are json.Number values of encoding/json, so that none loses
// precision; ValidateAs decodes the data into a Go type instead.
//
// A value that breaks the schema is a *SchemaValidationError, a schema that
// cannot be compiled an *InvalidSchemaFormatError, an envelope that cannot
// be applied a *jmespath.EnvelopeError.
func Validate(event, schema any, opts ...Option) (any, error) {
	o := newOptions(opts)
	s, err := schemaFor(context.Background(), schema, &o)
	if err != nil {
		return nil, err
	}
	return s.validate(event, &o)
}

// ValidateAs is Validate followed by decoding the validated data, defaults
// included, into a T.
func ValidateAs[T any](event, schema any, opts ...Option) (T, error) {
	var out T
	data, err := Validate(event, schema, opts...)
	if err != nil {
		return out, err
	}
	if err := convert(data, &out); err != nil {
		return out, err
	}
	return out, nil
}

// Validate checks event against the schema; see the Validate function. The
// options that shape compilation are ignored.
func (s *Schema) Validate(event any, opts ...Option) (any, error) {
	o := newOptions(opts)
	return s.validate(event, &o)
}

func (s *Schema) validate(event any, o *options) (any, error) {
	var expr *jmespath.Expression
	if o.envelope != "" {
		var err error
		if expr, err = o.expression(); err != nil {
			return nil, err
		}
	}
	data, _, err := s.check(event, expr, o.defaults)
	return data, err
}

// check unwraps event with expr, when there is one, validates what it
// selects against s, when there is one, and fills in the defaults. It
// reports whether a default was added.
func (s *Schema) check(event any, expr *jmespath.Expression, defaults bool) (data any, changed bool, err error) {
	if expr != nil {
		debug.Logger().Debug("Envelope detected", "envelope", expr.String())
		if event, err = expr.Search(event); err != nil {
			return nil, false, err
		}
	}
	if data, err = normalize(event); err != nil {
		return nil, false, err
	}
	if s == nil {
		return data, false, nil
	}
	if err = s.compiled.Validate(data); err != nil {
		return nil, false, s.validationError(data, err)
	}
	if !defaults {
		return data, false, nil
	}
	data, changed = applyDefaults(s.compiled, data)
	return data, changed, nil
}

// schemaFor returns the compiled form of schema, from the cache when it was
// compiled before with the same options.
func schemaFor(ctx context.Context, schema any, o *options) (*Schema, error) {
	if s, ok := schema.(*Schema); ok {
		if s == nil {
			return nil, &InvalidSchemaFormatError{Err: errNilSchema}
		}
		return s, nil
	}
	raw, err := sourceJSON(schema)
	if err != nil {
		return nil, &InvalidSchemaFormatError{Formats: o.formatNames(), Err: err}
	}
	key, cacheable := o.compileKey(raw)
	if cacheable {
		if s, ok := schemas.get(key); ok {
			return s, nil
		}
	}
	s, err := compile(ctx, raw, o)
	if err != nil {
		return nil, err
	}
	if cacheable {
		schemas.put(key, s)
	}
	return s, nil
}

// errNilSchema reports a nil *Schema given as the schema.
var errNilSchema = errors.New("nil *Schema")

// expression returns the compiled envelope, shared by every call with the
// same expression when no custom function is involved.
func (o *options) expression() (*jmespath.Expression, error) {
	shared := len(o.jmespathOptions) == 0
	if shared {
		if e, ok := envelopes.get(o.envelope); ok {
			return e, nil
		}
	}
	e, err := jmespath.Compile(o.envelope, o.jmespathOptions...)
	if err != nil {
		return nil, err
	}
	if shared {
		envelopes.put(o.envelope, e)
	}
	return e, nil
}

func compile(ctx context.Context, raw []byte, o *options) (*Schema, error) {
	fail := func(err error) (*Schema, error) {
		return nil, &InvalidSchemaFormatError{Schema: string(raw), Formats: o.formatNames(), Err: err}
	}
	doc, err := decode(raw)
	if err != nil {
		return fail(err)
	}
	c, err := newCompiler(o)
	if err != nil {
		return fail(err)
	}
	docs := map[string]any{rootURL: doc}
	loaders := o.urlLoaders()
	loader := &recordingLoader{
		load: func(scheme, location string) (any, error) {
			load, ok := loaders[scheme]
			if !ok {
				return nil, fmt.Errorf("%w %q", ErrNoLoader, scheme)
			}
			return load(ctx, location)
		},
		docs: docs,
	}
	c.UseLoader(loader)
	for _, r := range o.resources {
		if err = addResource(c, docs, r); err != nil {
			return fail(err)
		}
	}
	if err = c.AddResource(rootURL, doc); err != nil {
		return fail(err)
	}
	compiled, err := c.Compile(rootURL)
	if err != nil {
		// The engine flattens the errors of loaders into text: report the
		// loader's own, so that errors.Is reaches it.
		if loader.err != nil {
			err = loader.err
		}
		return fail(err)
	}
	if !o.formatAssertion {
		dropFormats(compiled)
	}
	return &Schema{compiled: compiled, docs: docs}, nil
}

func newCompiler(o *options) (*jsonschema.Compiler, error) {
	draft, err := o.draft.engine()
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(draft)
	// fastjsonschema asserts formats and the content keywords whatever the
	// draft; the specifications of 2019-09 and later make them annotations.
	c.AssertFormat()
	c.AssertContent()
	if o.regexpEngine != nil {
		engine := o.regexpEngine
		c.UseRegexpEngine(func(pattern string) (jsonschema.Regexp, error) {
			re, compileErr := engine(pattern)
			if compileErr != nil {
				return nil, compileErr
			}
			return re, nil
		})
	}
	for _, f := range o.formats {
		check := f.check
		if check == nil {
			if check, err = regexpFormat(f.pattern); err != nil {
				return nil, err
			}
		}
		c.RegisterFormat(&jsonschema.Format{Name: f.name, Validate: formatValidator(f.name, check)})
	}
	return c, nil
}

// errFormat is what a custom format reports for a value it rejects.
var errFormat = errors.New("value does not conform to the format")

// formatValidator applies check to string values only, as fastjsonschema
// does.
func formatValidator(name string, check func(string) bool) func(any) error {
	return func(v any) error {
		s, ok := v.(string)
		if !ok || check(s) {
			return nil
		}
		return fmt.Errorf("%w %s", errFormat, name)
	}
}

// errUnsupportedDraft reports a Draft value this package does not know.
var errUnsupportedDraft = errors.New("unsupported draft")

func (d Draft) engine() (*jsonschema.Draft, error) {
	switch d {
	case Draft4:
		return jsonschema.Draft4, nil
	case Draft6:
		return jsonschema.Draft6, nil
	case Draft7:
		return jsonschema.Draft7, nil
	case Draft2019:
		return jsonschema.Draft2019, nil
	case Draft2020:
		return jsonschema.Draft2020, nil
	default:
		return nil, fmt.Errorf("%w: %d", errUnsupportedDraft, int(d))
	}
}

func addResource(c *jsonschema.Compiler, docs map[string]any, r resource) error {
	raw, err := sourceJSON(r.schema)
	if err != nil {
		return fmt.Errorf("resource %s: %w", r.url, err)
	}
	doc, err := decode(raw)
	if err != nil {
		return fmt.Errorf("resource %s: %w", r.url, err)
	}
	if err := c.AddResource(r.url, doc); err != nil {
		return fmt.Errorf("resource %s: %w", r.url, err)
	}
	location, _, _ := strings.Cut(r.url, "#")
	docs[location] = doc
	return nil
}

// decode parses JSON keeping numbers exact, as json.Number values: the
// engine compares numbers as rationals.
func decode(raw []byte) (any, error) {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return v, nil
}

// normalize turns the data to validate into a fresh decoded JSON value,
// which the validation may then fill with defaults without touching the
// caller's data.
func normalize(event any) (any, error) {
	var raw []byte
	switch v := event.(type) {
	case jsontext.Value:
		raw = v
	case []byte:
		raw = v
	default:
		var err error
		if raw, err = json.Marshal(event, jsontext.AllowInvalidUTF8(true)); err != nil {
			return nil, fmt.Errorf("%w: marshal %T: %w", ErrInvalidEvent, event, err)
		}
	}
	data, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	return data, nil
}

// convert decodes the JSON value data into out.
func convert(data, out any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("validation: marshal the validated data: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("validation: decode the validated data into %T: %w", out, err)
	}
	return nil
}

// recordingLoader resolves remote references through the loaders of their
// scheme and keeps the documents it loads, to report definitions from them.
type recordingLoader struct {
	load func(scheme, location string) (any, error)
	docs map[string]any
	err  error
}

// Load implements jsonschema.URLLoader.
func (l *recordingLoader) Load(location string) (any, error) {
	doc, err := l.fetch(location)
	if err != nil {
		if l.err == nil {
			l.err = fmt.Errorf("load %s: %w", location, err)
		}
		return nil, err
	}
	l.docs[location] = doc
	return doc, nil
}

func (l *recordingLoader) fetch(location string) (any, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	doc, err := l.load(strings.ToLower(u.Scheme), location)
	if err != nil {
		return nil, err
	}
	raw, err := sourceJSON(doc)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}
