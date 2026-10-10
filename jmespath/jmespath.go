// Package jmespath extracts data from events with JMESPath expressions, plus
// the functions Powertools adds to decode what AWS event sources encode:
//
//   - powertools_json(string) parses a JSON string;
//   - powertools_base64(string) decodes base64 into a UTF-8 string;
//   - powertools_base64_gzip(string) decodes base64, then gunzips it.
//
// The envelope constants are the expressions that unwrap the payload of the
// common event sources, as Powertools Python names them. Validation,
// idempotency, the logger's correlation id and feature flags all accept these
// expressions.
//
// Expressions follow JMESPath Community Edition, a superset of the JMESPath
// specification Powertools Python implements.
package jmespath

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	jp "github.com/jmespath-community/go-jmespath"
)

// The envelopes of the common event sources.
const (
	APIGatewayREST           = "powertools_json(body)"
	APIGatewayHTTP           = APIGatewayREST
	SQS                      = "Records[*].powertools_json(body)"
	SNS                      = "Records[0].Sns.Message | powertools_json(@)"
	EventBridge              = "detail"
	CloudWatchEventsSchedule = EventBridge
	KinesisDataStream        = "Records[*].kinesis.powertools_json(powertools_base64(data))"
	CloudWatchLogs           = "awslogs.powertools_base64_gzip(data) | powertools_json(@).logEvents[*]"
	S3SNSSQS                 = "Records[*].powertools_json(body).powertools_json(Message).Records[0]"
	S3SQS                    = "Records[*].powertools_json(body).Records[0]"
	S3SNSKinesisFirehose     = "records[*].powertools_json(powertools_base64(data)).powertools_json(Message).Records[0]"
	S3KinesisFirehose        = "records[*].powertools_json(powertools_base64(data)).Records[0]"
	S3EventBridgeSQS         = "Records[*].powertools_json(body).detail"
)

// ErrInvalidEnvelope is what every failure to compile or evaluate an
// expression matches with errors.Is.
var ErrInvalidEnvelope = errors.New("invalid envelope expression")

// EnvelopeError reports an expression that could not be compiled, or could
// not be evaluated against the data it was given.
type EnvelopeError struct {
	Expression string
	Err        error
}

func (e *EnvelopeError) Error() string {
	return fmt.Sprintf("failed to unwrap event from envelope using expression %q: %v", e.Expression, e.Err)
}

// Unwrap returns the cause.
func (e *EnvelopeError) Unwrap() error { return e.Err }

// Is makes every EnvelopeError match ErrInvalidEnvelope.
func (*EnvelopeError) Is(target error) bool { return target == ErrInvalidEnvelope }

// Type is the type of a custom function's argument.
type Type string

// The argument types a custom function can declare.
const (
	TypeAny         Type = "any"
	TypeString      Type = "string"
	TypeNumber      Type = "number"
	TypeArray       Type = "array"
	TypeObject      Type = "object"
	TypeArrayString Type = "array[string]"
	TypeArrayNumber Type = "array[number]"
	TypeExpref      Type = "expref"
)

// Arg declares one argument of a custom function.
type Arg struct {
	// Types lists the types the argument accepts.
	Types []Type
	// Variadic lets the last argument repeat.
	Variadic bool
	// Optional lets the argument be left out.
	Optional bool
}

// Function is a custom JMESPath function, for formats the built-in functions
// do not decode. It receives the arguments decoded as JSON values (nil,
// bool, float64, string, []any, map[string]any) and returns one.
type Function struct {
	Name string
	Args []Arg
	Call func(args []any) (any, error)
}

// Option configures how an expression is compiled.
type Option func(*options)

type options struct {
	functions []Function
}

// WithFunctions adds custom functions next to the built-in and the
// Powertools ones.
func WithFunctions(functions ...Function) Option {
	return func(o *options) { o.functions = append(o.functions, functions...) }
}

// Expression is a compiled JMESPath expression. It is safe for concurrent
// use: compile it once, at package level or in init, and search many times.
type Expression struct {
	source   string
	compiled jp.JMESPath
}

// Compile parses expression, with the Powertools functions and any custom
// one available to it.
func Compile(expression string, opts ...Option) (*Expression, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	compiled, err := jp.Compile(expression, entries(o.functions)...)
	if err != nil {
		return nil, &EnvelopeError{Expression: expression, Err: err}
	}
	return &Expression{source: expression, compiled: compiled}, nil
}

// MustCompile is Compile for expressions known to be valid; it panics
// otherwise.
func MustCompile(expression string, opts ...Option) *Expression {
	e, err := Compile(expression, opts...)
	if err != nil {
		panic(err)
	}
	return e
}

// String returns the expression's source.
func (e *Expression) String() string { return e.source }

// Search evaluates the expression against data. Data is a decoded JSON
// value, a struct or map that marshals to JSON (an aws-lambda-go event), or
// raw JSON ([]byte, jsontext.Value). It returns the selected value as a
// decoded JSON value, nil when nothing matches.
func (e *Expression) Search(data any) (any, error) {
	generic, err := Generic(data)
	if err != nil {
		return nil, &EnvelopeError{Expression: e.source, Err: err}
	}
	result, err := e.compiled.Search(generic)
	if err != nil {
		return nil, &EnvelopeError{Expression: e.source, Err: err}
	}
	return result, nil
}

// Query evaluates expression against data and returns what it selects:
// Powertools Python's jmespath_utils.query.
func Query(data any, expression string, opts ...Option) (any, error) {
	e, err := Compile(expression, opts...)
	if err != nil {
		return nil, err
	}
	return e.Search(data)
}

// QueryAs evaluates expression against data and decodes the selection into
// a T.
func QueryAs[T any](data any, expression string, opts ...Option) (T, error) {
	var out T
	selected, err := Query(data, expression, opts...)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(selected)
	if err != nil {
		return out, &EnvelopeError{Expression: expression, Err: err}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, &EnvelopeError{Expression: expression, Err: fmt.Errorf("decode the selection into %T: %w", out, err)}
	}
	return out, nil
}

// Generic turns data into the decoded JSON value JMESPath works on: raw JSON
// is parsed, anything else is marshalled and parsed back, so that structs
// are seen through their JSON field names.
func Generic(data any) (any, error) {
	var raw []byte
	switch v := data.(type) {
	case jsontext.Value:
		raw = v
	case []byte:
		raw = v
	case string, bool, float64:
		return v, nil
	default:
		var err error
		if raw, err = json.Marshal(v); err != nil {
			return nil, fmt.Errorf("marshal %T: %w", data, err)
		}
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return generic, nil
}

// entries turns custom functions into the library's, after the Powertools
// ones.
func entries(custom []Function) []jp.FunctionEntry {
	all := append(powertoolsFunctions(), custom...)
	out := make([]jp.FunctionEntry, 0, len(all))
	for _, f := range all {
		args := make([]jp.ArgSpec, 0, len(f.Args))
		for _, a := range f.Args {
			types := make([]jp.JpType, 0, len(a.Types))
			for _, t := range a.Types {
				types = append(types, jp.JpType(t))
			}
			args = append(args, jp.ArgSpec{Types: types, Variadic: a.Variadic, Optional: a.Optional})
		}
		out = append(out, jp.FunctionEntry{Name: f.Name, Arguments: args, Handler: f.Call})
	}
	return out
}

// stringArg is the signature of the three Powertools functions.
var stringArg = []Arg{{Types: []Type{TypeString}}}

func powertoolsFunctions() []Function {
	return []Function{
		{Name: "powertools_json", Args: stringArg, Call: decodeJSON},
		{Name: "powertools_base64", Args: stringArg, Call: decodeBase64},
		{Name: "powertools_base64_gzip", Args: stringArg, Call: decodeBase64Gzip},
	}
}

// Errors of the Powertools functions.
var (
	// ErrNotUTF8 reports decoded bytes that are not UTF-8 text.
	ErrNotUTF8 = errors.New("decoded data is not valid UTF-8")
	// ErrTooLarge reports gzip data that inflates beyond MaxDecompressedSize.
	ErrTooLarge = errors.New("decompressed data is too large")
)

// MaxDecompressedSize bounds what powertools_base64_gzip inflates, so that a
// small malicious payload cannot exhaust the function's memory.
const MaxDecompressedSize = 128 << 20

// errNotString reports a Powertools function called with something other
// than a string; the declared signature already rules it out.
var errNotString = errors.New("argument is not a string")

// firstString returns the only argument of a Powertools function.
func firstString(name string, args []any) (string, error) {
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			return s, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, errNotString)
}

func decodeJSON(args []any) (any, error) {
	s, err := firstString("powertools_json", args)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("powertools_json: %w", err)
	}
	return out, nil
}

func decodeBase64(args []any) (any, error) {
	s, err := firstString("powertools_base64", args)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("powertools_base64: %w", err)
	}
	if !utf8.Valid(decoded) {
		return nil, fmt.Errorf("powertools_base64: %w", ErrNotUTF8)
	}
	return string(decoded), nil
}

func decodeBase64Gzip(args []any) (any, error) {
	s, err := firstString("powertools_base64_gzip", args)
	if err != nil {
		return nil, err
	}
	compressed, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("powertools_base64_gzip: %w", err)
	}
	decoded, err := inflate(compressed, MaxDecompressedSize)
	if err != nil {
		return nil, fmt.Errorf("powertools_base64_gzip: %w", err)
	}
	if !utf8.Valid(decoded) {
		return nil, fmt.Errorf("powertools_base64_gzip: %w", ErrNotUTF8)
	}
	return string(decoded), nil
}

// inflate gunzips data, refusing to produce more than limit bytes.
func inflate(data []byte, limit int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = reader.Close() }() //nolint:errcheck // a read-only gzip reader has nothing to report on close
	decoded, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("inflate: %w", err)
	}
	if int64(len(decoded)) > limit {
		return nil, ErrTooLarge
	}
	return decoded, nil
}
