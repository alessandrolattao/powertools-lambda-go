package validation

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// Draft is a JSON Schema specification version. A schema's "$schema"
// keyword picks its draft; the default draft applies to schemas without one.
type Draft int

// The drafts the validator implements.
const (
	Draft4    Draft = 4
	Draft6    Draft = 6
	Draft7    Draft = 7
	Draft2019 Draft = 2019
	Draft2020 Draft = 2020
)

// DefaultDraft is the draft of schemas that do not declare "$schema".
// Powertools Python's engine, fastjsonschema, labels its fallback generator
// 2019-09, but that generator implements the draft-07 keywords (plus the
// uuid and duration formats, which every draft accepts here): Draft 7 is
// what Python actually applies.
const DefaultDraft = Draft7

// String returns the draft's name, as JSON Schema spells it.
func (d Draft) String() string {
	switch d {
	case Draft4, Draft6, Draft7:
		return fmt.Sprintf("draft-%02d", int(d))
	case Draft2019:
		return "draft/2019-09"
	case Draft2020:
		return "draft/2020-12"
	default:
		return fmt.Sprintf("draft(%d)", int(d))
	}
}

// Regexp is a compiled regular expression, as a custom regexp engine
// returns it.
type Regexp interface {
	// MatchString reports whether s contains a match.
	MatchString(s string) bool
	// String returns the source of the expression.
	String() string
}

// Loader returns the JSON Schema document at url, for a "$ref" to a
// document the schema does not embed. The document is raw JSON ([]byte,
// jsontext.Value or string) or a value that marshals to JSON, such as a
// map[string]any. The context is that of the invocation that compiles the
// schema, or context.Background() for Compile and Validate.
type Loader func(ctx context.Context, url string) (any, error)

// Option configures how a schema is compiled and how data is validated.
//
// Options that shape the compiled schema (formats, loaders, resources, the
// default draft, the regexp engine) are ignored when the schema given to
// Validate is already a compiled *Schema.
type Option func(*options)

type options struct {
	envelope        string
	jmespathOptions []jmespath.Option

	defaults        bool
	formatAssertion bool
	draft           Draft
	formats         []customFormat
	loaders         map[string]Loader
	httpClient      *http.Client
	httpLoader      bool
	resources       []resource
	regexpEngine    func(pattern string) (Regexp, error)

	// opaque is set by the compile options that carry functions: Go cannot
	// compare them, so a schema compiled with them is not cached by its
	// source. An envelope with custom functions is not cached either, for
	// the same reason (see expression).
	opaque bool
}

type customFormat struct {
	name    string
	pattern string // the source of a regular expression format, "" for a function
	check   func(string) bool
}

type resource struct {
	url    string
	schema any
}

func newOptions(opts []Option) options {
	o := options{defaults: true, formatAssertion: true, draft: DefaultDraft}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithEnvelope selects, with a JMESPath expression, the part of the event to
// validate, as Powertools Python's envelope parameter does. The expression
// can call the Powertools functions (powertools_json, powertools_base64,
// powertools_base64_gzip) and the custom functions in opts. The built-in
// envelopes are the constants of this package.
func WithEnvelope(expression string, opts ...jmespath.Option) Option {
	return func(o *options) {
		o.envelope = expression
		o.jmespathOptions = opts
	}
}

// WithDefaults turns the injection of schema defaults on or off. It is on by
// default, like fastjsonschema's use_default: a property missing from the
// data, whose schema declares a "default", is added to the validated data
// with that value.
func WithDefaults(enabled bool) Option {
	return func(o *options) { o.defaults = enabled }
}

// WithFormatAssertion turns the checking of the "format" keyword on or off.
// It is on by default for every draft, as fastjsonschema's use_formats,
// although the 2019-09 and 2020-12 specifications make formats annotations
// only.
func WithFormatAssertion(enabled bool) Option {
	return func(o *options) { o.formatAssertion = enabled }
}

// WithDefaultDraft sets the draft of schemas that do not declare "$schema".
// It defaults to DefaultDraft.
func WithDefaultDraft(d Draft) Option {
	return func(o *options) { o.draft = d }
}

// WithFormat adds a custom format: check receives every string value whose
// schema names the format and reports whether it conforms. Values that are
// not strings always conform. A custom format replaces a built-in format of
// the same name, except "regex".
func WithFormat(name string, check func(value string) bool) Option {
	return func(o *options) {
		o.formats = append(o.formats, customFormat{name: name, check: check})
		o.opaque = true
	}
}

// WithFormatRegexp adds a custom format defined by a regular expression. As
// in Python, where the format is applied with re.match, the expression is
// anchored at the start of the value only: add "$" to anchor its end too.
func WithFormatRegexp(name, pattern string) Option {
	return func(o *options) {
		o.formats = append(o.formats, customFormat{name: name, pattern: pattern})
	}
}

// WithLoader resolves a "$ref" to a remote document whose URL has the given
// scheme ("https", "s3", "urn") through load: the Go side of Python's
// handlers parameter. No remote document is fetched unless a loader for its
// scheme is set.
func WithLoader(scheme string, load Loader) Option {
	return func(o *options) {
		if o.loaders == nil {
			o.loaders = map[string]Loader{}
		}
		o.loaders[strings.ToLower(scheme)] = load
		o.opaque = true
	}
}

// WithHTTPLoader resolves a "$ref" to an http or https URL with a GET
// through client, which fastjsonschema does by default and this package
// only on request. A nil client means one with a 10 second timeout. A
// loader set with WithLoader for the same scheme wins.
func WithHTTPLoader(client *http.Client) Option {
	return func(o *options) {
		o.httpLoader = true
		o.httpClient = client
	}
}

// WithResource makes the schema document available at url to the "$ref"s
// of the schema, without fetching it: the way to split a schema into
// documents that ship with the function. The document is raw JSON or a
// value that marshals to JSON.
func WithResource(url string, schema any) Option {
	return func(o *options) {
		o.resources = append(o.resources, resource{url: url, schema: schema})
	}
}

// WithRegexpEngine replaces Go's regexp package for the "pattern" and
// "patternProperties" keywords and the "regex" format. Go's engine is RE2,
// without lookarounds and backreferences: an engine such as
// github.com/dlclark/regexp2 accepts the patterns Python's re module does.
func WithRegexpEngine(compile func(pattern string) (Regexp, error)) Option {
	return func(o *options) {
		o.regexpEngine = compile
		o.opaque = true
	}
}

// compileKey identifies a compilation of the schema source raw under these
// options, for the cache of schemas compiled by Validate. It reports false
// when the options carry functions, which cannot be part of a key.
func (o *options) compileKey(raw []byte) (string, bool) {
	if o.opaque {
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "draft=%d formats=%t", o.draft, o.formatAssertion)
	for _, f := range o.formats {
		fmt.Fprintf(&b, " format=%q:%q", f.name, f.pattern)
	}
	if o.httpLoader {
		fmt.Fprintf(&b, " http=%p", o.httpClient)
	}
	for _, r := range o.resources {
		doc, err := sourceJSON(r.schema)
		if err != nil {
			return "", false
		}
		fmt.Fprintf(&b, " resource=%q:%q", r.url, doc)
	}
	b.WriteString("\x00")
	b.Write(raw)
	return b.String(), true
}

// formatNames lists the custom formats, for error messages.
func (o *options) formatNames() []string {
	names := make([]string, 0, len(o.formats))
	for _, f := range o.formats {
		names = append(names, f.name)
	}
	return names
}

// regexpFormat compiles a format pattern the way Python's re.match applies
// it: anchored at the start of the value.
func regexpFormat(pattern string) (func(string) bool, error) {
	re, err := regexp.Compile(`^(?:` + pattern + `)`)
	if err != nil {
		return nil, fmt.Errorf("format pattern %q: %w", pattern, err)
	}
	return re.MatchString, nil
}

// sourceJSON returns the JSON text of a schema document given as raw JSON
// or as a value to marshal.
func sourceJSON(doc any) ([]byte, error) {
	switch v := doc.(type) {
	case []byte:
		return v, nil
	case jsontext.Value:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		raw, err := json.Marshal(doc, json.Deterministic(true))
		if err != nil {
			return nil, fmt.Errorf("marshal %T: %w", doc, err)
		}
		return raw, nil
	}
}
