package datamasking

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The masking engine works on decoded JSON values: nil, bool, int64,
// float64, string, []any and map[string]any. Integers stay int64, apart
// from float64, because Python tells them apart (str(30) is "30",
// str(30.0) is "30.0") and the masks depend on that text.

// generic turns data into the value the engine works on. Raw JSON ([]byte,
// jsontext.Value) is parsed; a string is parsed only when parseString is
// set, since a plain string is a value of its own for whole-data erase; any
// other Go value is marshalled with encoding/json/v2 and parsed back, so
// that structs are seen through their JSON field names and the caller's
// data is never modified.
func generic(data any, parseString bool, opts []json.Options) (any, error) {
	switch v := data.(type) {
	case nil:
		return nil, nil //nolint:nilnil // nil is JSON null, a value like any other
	case jsontext.Value:
		return decodeJSON(v)
	case []byte:
		return decodeJSON(v)
	case string:
		if parseString {
			return decodeJSON([]byte(v))
		}
		return v, nil
	}
	raw, err := json.Marshal(data, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %T: %w", ErrUnsupportedType, data, err)
	}
	return decodeJSON(raw)
}

// errUnexpectedToken reports a token jsontext would never return where a
// value starts.
var errUnexpectedToken = errors.New("unexpected token")

// decodeJSON parses one JSON document. Duplicate names are accepted, the
// last one wins, as Python's json.loads does.
func decodeJSON(raw []byte) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true))
	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("%w: parse JSON: %w", ErrUnsupportedType, err)
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: parse JSON: data after the top-level value", ErrUnsupportedType)
	}
	return v, nil
}

func decodeValue(dec *jsontext.Decoder) (any, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err //nolint:wrapcheck // decodeJSON wraps it once for the whole document
	}
	switch tok.Kind() {
	case 'n':
		return nil, nil //nolint:nilnil // nil is JSON null, a value like any other
	case 't', 'f':
		return tok.Bool(), nil
	case '"':
		return tok.String(), nil
	case '0':
		return number(tok.String())
	case '{':
		return decodeObject(dec)
	case '[':
		return decodeArray(dec)
	default:
		return nil, fmt.Errorf("%w %v", errUnexpectedToken, tok.Kind())
	}
}

func decodeObject(dec *jsontext.Decoder) (any, error) {
	obj := map[string]any{}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, err //nolint:wrapcheck // decodeJSON wraps it once for the whole document
		}
		name := tok.String() // the token is only valid until the next read
		value, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		obj[name] = value
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err //nolint:wrapcheck // decodeJSON wraps it once for the whole document
	}
	return obj, nil
}

func decodeArray(dec *jsontext.Decoder) (any, error) {
	arr := []any{}
	for dec.PeekKind() != ']' {
		value, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, value)
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, err //nolint:wrapcheck // decodeJSON wraps it once for the whole document
	}
	return arr, nil
}

// number keeps an integer literal as int64, as Python keeps it an int, and
// reads anything else (a fraction, an exponent, an integer beyond int64) as
// float64.
func number(literal string) (any, error) {
	if !strings.ContainsAny(literal, ".eE") {
		if n, err := strconv.ParseInt(literal, 10, 64); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return nil, fmt.Errorf("number %s: %w", literal, err)
	}
	return f, nil
}

// encodeJSON marshals a value of the engine. Keys are sorted, so that the
// same data always produces the same bytes.
func encodeJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedType, err)
	}
	return raw, nil
}

// decodeInto turns a value of the engine into the caller's type T. Raw JSON
// targets ([]byte, jsontext.Value) receive the document itself.
func decodeInto[T any](v any, opts []json.Options) (T, error) {
	var out T
	raw, err := encodeJSON(v)
	if err != nil {
		return out, err
	}
	return unmarshalInto[T](raw, opts)
}

// unmarshalInto decodes a JSON document into T, handing raw JSON targets
// the document itself rather than base64-decoding it.
func unmarshalInto[T any](raw []byte, opts []json.Options) (T, error) {
	var out T
	switch target := any(&out).(type) {
	case *[]byte:
		*target = bytes.Clone(raw)
		return out, nil
	case *jsontext.Value:
		*target = jsontext.Value(bytes.Clone(raw))
		return out, nil
	}
	if err := json.Unmarshal(raw, &out, opts...); err != nil {
		return out, fmt.Errorf("%w: decode into %T: %w", ErrUnsupportedType, out, err)
	}
	return out, nil
}
