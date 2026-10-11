package schema

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
)

// Number is a JSON number, kept as its text so that no precision is lost.
type Number string

// errTrailing reports data after the JSON value.
var errTrailing = errors.New("data after the JSON value")

// Decode decodes JSON into the values Validate reads: nil, bool, Number,
// string, []any and map[string]any. As encoding/json, it accepts duplicate
// names (the last one wins) and replaces invalid UTF-8.
func Decode(data []byte) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err = dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errTrailing
	}
	return v, nil
}

func decodeValue(dec *jsontext.Decoder) (any, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	switch tok.Kind() {
	case 'n':
		return nil, nil //nolint:nilnil // JSON null decodes to the nil value
	case 't', 'f':
		return tok.Bool(), nil
	case '"':
		return tok.String(), nil
	case '0':
		return Number(tok.String()), nil
	case '[':
		return decodeArray(dec)
	default: // '{'
		return decodeObject(dec)
	}
}

func decodeArray(dec *jsontext.Decoder) (any, error) {
	arr := []any{}
	for dec.PeekKind() != ']' {
		item, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, item)
	}
	_, err := dec.ReadToken()
	return arr, wrap(err)
}

func decodeObject(dec *jsontext.Decoder) (any, error) {
	obj := map[string]any{}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("decode JSON: %w", err)
		}
		// The token is only valid until the next read.
		name := tok.String()
		value, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		obj[name] = value
	}
	_, err := dec.ReadToken()
	return obj, wrap(err)
}

func wrap(err error) error {
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// normalize turns the numbers of a decoded document into Numbers.
func normalize(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = normalize(item)
		}
		return out
	default:
		if n, ok := number(v); ok {
			return n
		}
		return v
	}
}

// number returns the JSON number v holds, whatever its Go type.
func number(v any) (Number, bool) {
	switch x := v.(type) {
	case Number:
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", false
		}
		return Number(strconv.FormatFloat(x, 'g', -1, 64)), true
	case float32:
		return number(float64(x))
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return Number(strconv.FormatInt(rv.Int(), 10)), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Number(strconv.FormatUint(rv.Uint(), 10)), true
	default:
		return "", false
	}
}

// rat returns the exact value of a number.
func rat(v any) (*big.Rat, bool) {
	n, ok := number(v)
	if !ok {
		return nil, false
	}
	return new(big.Rat).SetString(string(n))
}

// typeOf names the JSON type of v; every number is a "number", as the
// failures of type say.
func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	if _, ok := number(v); ok {
		return "number"
	}
	return "invalid"
}

// hasType reports whether v is of one of the types; "integer" accepts any
// number without a fractional part (1.0 included).
func hasType(types []string, v any) bool {
	t := typeOf(v)
	for _, want := range types {
		if want == t {
			return true
		}
		if want == "integer" && t == "number" {
			if r, ok := rat(v); ok && r.IsInt() {
				return true
			}
		}
	}
	return false
}

// equal compares two JSON values, numbers by value.
func equal(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && equalObjects(x, y)
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, equal)
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	}
	ra, okA := rat(a)
	rb, okB := rat(b)
	return okA && okB && ra.Cmp(rb) == 0
}

func equalObjects(x, y map[string]any) bool {
	if len(x) != len(y) {
		return false
	}
	for k, xv := range x {
		if yv, ok := y[k]; !ok || !equal(xv, yv) {
			return false
		}
	}
	return true
}
