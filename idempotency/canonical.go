package idempotency

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// number is a JSON number exactly as the payload wrote it. Keeping the
// literal tells integers from floats, as Python's json.loads does, and keeps
// integers of any size exact.
type number string

// decodeTree parses raw JSON into the generic tree the canonical encoder
// works on: nil, bool, string, number, map[string]any and []any.
func decodeTree(raw []byte) (any, error) {
	keepNumbers := json.UnmarshalFromFunc(func(dec *jsontext.Decoder, v *any) error {
		if dec.PeekKind() != jsontext.KindNumber {
			return errors.ErrUnsupported // not a number: decode it as usual
		}
		literal, err := dec.ReadValue()
		if err != nil {
			return err //nolint:wrapcheck // the decoder reports its own position
		}
		*v = number(literal)
		return nil
	})
	var tree any
	if err := json.Unmarshal(raw, &tree, json.WithUnmarshalers(keepNumbers)); err != nil {
		return nil, fmt.Errorf("parse the payload: %w", err)
	}
	return tree, nil
}

// canonicalJSON renders v the way Python's json.dumps(v, sort_keys=True)
// does with its default settings, which is what Powertools Python hashes:
// object members sorted by name, ", " and ": " separators, every character
// outside printable ASCII escaped as \uXXXX, floats as Python's repr prints
// them. Equal bytes on both sides are what lets a Go and a Python function
// compute the same idempotency key.
func canonicalJSON(v any) ([]byte, error) {
	return appendCanonical(nil, v)
}

func appendCanonical(dst []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, v), nil
	case string:
		return appendPythonString(dst, v), nil
	case number:
		return append(dst, pythonNumber(string(v))...), nil
	case float64:
		return append(dst, pythonFloat64(v)...), nil
	case map[string]any:
		return appendCanonicalObject(dst, v)
	case []any:
		dst = append(dst, '[')
		for i, item := range v {
			if i > 0 {
				dst = append(dst, ", "...)
			}
			var err error
			if dst, err = appendCanonical(dst, item); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	default:
		// A value a custom JMESPath function returned: render what it
		// marshals to.
		raw, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return nil, fmt.Errorf("marshal %T: %w", v, err)
		}
		tree, err := decodeTree(raw)
		if err != nil {
			return nil, err
		}
		return appendCanonical(dst, tree)
	}
}

func appendCanonicalObject(dst []byte, object map[string]any) ([]byte, error) {
	// Byte order of UTF-8 strings is code point order, which is how Python
	// sorts str keys.
	names := slices.Sorted(maps.Keys(object))
	dst = append(dst, '{')
	for i, name := range names {
		if i > 0 {
			dst = append(dst, ", "...)
		}
		dst = appendPythonString(dst, name)
		dst = append(dst, ": "...)
		var err error
		if dst, err = appendCanonical(dst, object[name]); err != nil {
			return nil, err
		}
	}
	return append(dst, '}'), nil
}

// appendPythonString quotes s as json.dumps does with ensure_ascii=True.
func appendPythonString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	appendU := func(dst []byte, r rune) []byte {
		return append(dst, '\\', 'u', hex[r>>12&0xf], hex[r>>8&0xf], hex[r>>4&0xf], hex[r&0xf])
	}
	dst = append(dst, '"')
	for _, r := range s {
		switch {
		case r == '"':
			dst = append(dst, '\\', '"')
		case r == '\\':
			dst = append(dst, '\\', '\\')
		case r == '\n':
			dst = append(dst, '\\', 'n')
		case r == '\r':
			dst = append(dst, '\\', 'r')
		case r == '\t':
			dst = append(dst, '\\', 't')
		case r == '\b':
			dst = append(dst, '\\', 'b')
		case r == '\f':
			dst = append(dst, '\\', 'f')
		case r >= ' ' && r <= '~':
			dst = append(dst, byte(r))
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			dst = appendU(appendU(dst, hi), lo)
		default:
			dst = appendU(dst, r)
		}
	}
	return append(dst, '"')
}

// pythonNumber renders a JSON number literal as Python does after
// json.loads: an integer literal stays an exact integer, anything with a
// fraction or an exponent becomes a float.
func pythonNumber(literal string) string {
	if !strings.ContainsAny(literal, ".eE") {
		if literal == "-0" {
			return "0"
		}
		return literal
	}
	// Out of range literals round to an infinity or to zero, as in Python;
	// the error only says so.
	f, _ := strconv.ParseFloat(literal, 64) //nolint:errcheck // a valid JSON number always parses, at worst out of range
	return pythonFloat(f)
}

// maxExactInteger bounds the float64 values pythonFloat64 renders as
// integers: those an int64 holds.
const maxExactInteger = float64(1 << 63)

// pythonFloat64 renders a number JMESPath selected. JMESPath works on
// float64, which cannot tell 1 from 1.0: an integral value renders as an
// integer, as the integer literal it almost always was; any other one as a
// Python float.
func pythonFloat64(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < maxExactInteger {
		return strconv.FormatInt(int64(f), 10)
	}
	return pythonFloat(f)
}

// pythonFloat renders f as Python's repr does: the shortest digits that
// read back as f, in scientific notation when the decimal exponent is below
// -4 or at least 16, otherwise in fixed notation with at least one
// fractional digit.
func pythonFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	scientific := strconv.FormatFloat(f, 'e', -1, 64)
	exponent, err := strconv.Atoi(scientific[strings.IndexByte(scientific, 'e')+1:])
	if err == nil && (exponent < -4 || exponent >= 16) {
		// Go and Python agree on this form: "1e+16", "1.5e-07".
		return scientific
	}
	fixed := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(fixed, ".") {
		fixed += ".0"
	}
	return fixed
}

// isMissingKey reports whether the data selected for the idempotency key
// holds nothing, by Powertools Python's rules: an object whose values are all
// null (a JMESPath multi-select keeps the names of missing fields), an array
// whose items are all null, and anything falsy except numbers and booleans,
// so that 0 and false are valid keys.
func isMissingKey(data any) bool {
	switch data := data.(type) {
	case nil:
		return true
	case map[string]any:
		for _, v := range data {
			if v != nil {
				return false
			}
		}
		return true
	case []any:
		for _, v := range data {
			if v != nil {
				return false
			}
		}
		return true
	case string:
		return data == ""
	default:
		return false
	}
}
