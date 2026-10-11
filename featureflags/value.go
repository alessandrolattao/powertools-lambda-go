package featureflags

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The rule engine works on JSON values the way Python's json module decodes
// them, because that is what Powertools Python evaluates conditions on:
//
//   - null is nil, true and false are bool, strings are string;
//   - a number written without a fraction or an exponent is an int64 (Python
//     int), any other number a float64 (Python float);
//   - an array is []any;
//   - an object is *object, which keeps its members in document order, as a
//     Python dict does. Rules are evaluated in that order: the first one that
//     matches wins.

// object is a JSON object that remembers the order of its members.
type object struct {
	keys   []string
	values map[string]any
}

func newObject() *object {
	return &object{values: make(map[string]any)}
}

// get returns the member named key.
func (o *object) get(key string) (any, bool) {
	v, ok := o.values[key]
	return v, ok
}

// getOr returns the member named key, or fallback when there is none, as
// Python's dict.get.
func (o *object) getOr(key string, fallback any) any {
	if v, ok := o.values[key]; ok {
		return v
	}
	return fallback
}

// set adds or replaces a member. A replaced member keeps its position, as a
// Python dict keeps it when json.loads meets a repeated name.
func (o *object) set(key string, value any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *object) len() int { return len(o.keys) }

// errTrailingData reports JSON input with more than one value.
var errTrailingData = errors.New("invalid character after the top-level value")

// decodeOptions read JSON as Python's json.loads does: a repeated object
// member is not an error, the last one wins.
var decodeOptions = jsontext.AllowDuplicateNames(true)

// decodeJSON decodes one JSON document into the value model.
func decodeJSON(data []byte) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(data), decodeOptions)
	value, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errTrailingData
		}
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return value, nil
}

func decodeValue(dec *jsontext.Decoder) (any, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	switch tok.Kind() {
	case 'n':
		return nil, nil //nolint:nilnil // JSON null is nil in the value model
	case 't', 'f':
		return tok.Bool(), nil
	case '"':
		return tok.String(), nil
	case '0':
		return parseNumber(tok.String())
	case '{':
		return decodeObject(dec)
	default: // '['
		return decodeArray(dec)
	}
}

func decodeObject(dec *jsontext.Decoder) (any, error) {
	obj := newObject()
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		// The token is only valid until the next read.
		name := tok.String()
		value, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		obj.set(name, value)
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return obj, nil
}

func decodeArray(dec *jsontext.Decoder) (any, error) {
	list := []any{}
	for dec.PeekKind() != ']' {
		value, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		list = append(list, value)
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return list, nil
}

// parseNumber turns a JSON number literal into an int64, as Python reads a
// literal without fraction and exponent, or a float64. An integer too large
// for int64 becomes a float64: Python would keep it exact.
func parseNumber(literal string) (any, error) {
	if !strings.ContainsAny(literal, ".eE") {
		if n, err := strconv.ParseInt(literal, 10, 64); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, fmt.Errorf("parse JSON number %q: %w", literal, err)
	}
	return f, nil
}

// encodeJSON writes a model value as JSON, objects in their order. Floats
// keep a fraction or an exponent, so that they decode as floats again.
func encodeJSON(value any) (jsontext.Value, error) {
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf)
	if err := encodeValue(enc, value); err != nil {
		return nil, err
	}
	return jsontext.Value(bytes.TrimSpace(buf.Bytes())), nil
}

func encodeValue(enc *jsontext.Encoder, value any) error {
	var err error
	switch v := value.(type) {
	case nil:
		err = enc.WriteToken(jsontext.Null)
	case bool:
		err = enc.WriteToken(jsontext.Bool(v))
	case string:
		err = enc.WriteToken(jsontext.String(v))
	case int64:
		err = enc.WriteToken(jsontext.Int(v))
	case float64:
		err = enc.WriteValue(jsontext.Value(formatFloatJSON(v)))
	case []any:
		err = encodeList(enc, v)
	case *object:
		err = encodeObject(enc, v)
	default:
		err = fmt.Errorf("%w: %T", errNotJSON, value)
	}
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}

func encodeList(enc *jsontext.Encoder, list []any) error {
	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	for _, item := range list {
		if err := encodeValue(enc, item); err != nil {
			return err
		}
	}
	if err := enc.WriteToken(jsontext.EndArray); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}

func encodeObject(enc *jsontext.Encoder, obj *object) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	for _, key := range obj.keys {
		if err := enc.WriteToken(jsontext.String(key)); err != nil {
			return fmt.Errorf("encode JSON: %w", err)
		}
		if err := encodeValue(enc, obj.values[key]); err != nil {
			return err
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}

// errNotJSON reports a value outside the model, or a float JSON cannot hold.
var errNotJSON = errors.New("value has no JSON representation")

// formatFloatJSON formats f so that it reads back as a float. JSON has no
// NaN nor infinities; they become null, which only an evaluation result can
// carry, never a document.
func formatFloatJSON(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

// truthy reports Python's truth value of v.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *object:
		return x.len() > 0
	default:
		return true
	}
}

// typeName is Python's type(v).__name__.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *object:
		return "dict"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// number is a Python int or float, bools included (Python's bool is an int).
type number struct {
	isFloat bool
	i       int64
	f       float64
}

func asNumber(v any) (number, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return number{i: 1}, true
		}
		return number{}, true
	case int64:
		return number{i: x}, true
	case float64:
		return number{isFloat: true, f: x}, true
	default:
		return number{}, false
	}
}

// compareNumbers compares a and b exactly, as Python compares an int with a
// float. unordered is true when a NaN takes part: every comparison is false.
func compareNumbers(a, b number) (cmp int, unordered bool) {
	switch {
	case !a.isFloat && !b.isFloat:
		return cmpInt(a.i, b.i), false
	case a.isFloat && b.isFloat:
		if math.IsNaN(a.f) || math.IsNaN(b.f) {
			return 0, true
		}
		return cmpFloat(a.f, b.f), false
	case a.isFloat:
		c, nan := compareIntFloat(b.i, a.f)
		return -c, nan
	default:
		return compareIntFloat(a.i, b.f)
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareIntFloat compares an int and a float without the rounding a
// conversion of the int to float64 would bring above 2^53.
func compareIntFloat(i int64, f float64) (int, bool) {
	switch {
	case math.IsNaN(f):
		return 0, true
	case f >= math.MaxInt64: // 2^63 and beyond, +Inf included
		return -1, false
	case f < math.MinInt64:
		return 1, false
	}
	whole := math.Trunc(f)
	if c := cmpInt(i, int64(whole)); c != 0 {
		return c, false
	}
	return cmpFloat(0, f-whole), false
}

// pyEqual is Python's == on JSON values: numbers compare by value whatever
// their type (True == 1 == 1.0), lists and dicts compare their members, and
// values of unrelated types are simply different.
func pyEqual(a, b any) bool {
	if na, ok := asNumber(a); ok {
		nb, ok := asNumber(b)
		if !ok {
			return false
		}
		c, unordered := compareNumbers(na, nb)
		return !unordered && c == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, pyEqual)
	case *object:
		y, ok := b.(*object)
		return ok && equalObjects(x, y)
	default:
		return false
	}
}

// equalObjects is == on dicts: the same keys with equal values, in any
// order.
func equalObjects(x, y *object) bool {
	if x.len() != y.len() {
		return false
	}
	for key, value := range x.values {
		other, ok := y.values[key]
		if !ok || !pyEqual(value, other) {
			return false
		}
	}
	return true
}

// The comparison operators of the ordering actions.
const (
	opLess         = "<"
	opLessEqual    = "<="
	opGreater      = ">"
	opGreaterEqual = ">="
)

// pyOrder is Python's a <op> b: numbers by value, strings by code point,
// lists lexicographically; anything else is a TypeError.
func pyOrder(a, b any, op string) (bool, error) {
	if na, ok := asNumber(a); ok {
		if nb, ok := asNumber(b); ok {
			c, unordered := compareNumbers(na, nb)
			return !unordered && applyOrder(c, op), nil
		}
	}
	switch x := a.(type) {
	case string:
		if y, ok := b.(string); ok {
			// UTF-8 preserves the order of code points.
			return applyOrder(strings.Compare(x, y), op), nil
		}
	case []any:
		if y, ok := b.([]any); ok {
			return orderLists(x, y, op)
		}
	}
	return false, typeErrorf("'%s' not supported between instances of '%s' and '%s'", op, typeName(a), typeName(b))
}

func applyOrder(c int, op string) bool {
	switch op {
	case opLess:
		return c < 0
	case opLessEqual:
		return c <= 0
	case opGreater:
		return c > 0
	default:
		return c >= 0
	}
}

// orderLists compares lists as Python does: the first members that differ
// decide, with the same operator; when one list is a prefix of the other,
// the lengths decide.
func orderLists(a, b []any, op string) (bool, error) {
	for i := range min(len(a), len(b)) {
		if !pyEqual(a[i], b[i]) {
			return pyOrder(a[i], b[i], op)
		}
	}
	return applyOrder(cmpInt(int64(len(a)), int64(len(b))), op), nil
}

// pyContains is Python's item in container: membership by == in a list, a
// substring in a string, a key in a dict. Other containers are a TypeError.
func pyContains(container, item any) (bool, error) {
	switch c := container.(type) {
	case []any:
		return slices.ContainsFunc(c, func(member any) bool { return pyEqual(member, item) }), nil
	case string:
		s, ok := item.(string)
		if !ok {
			return false, typeErrorf("'in <string>' requires string as left operand, not %s", typeName(item))
		}
		return strings.Contains(c, s), nil
	case *object:
		switch k := item.(type) {
		case []any, *object:
			return false, typeErrorf("unhashable type: '%s'", typeName(item))
		case string:
			_, ok := c.values[k]
			return ok, nil
		default:
			// Keys of a JSON object are strings, and no number, bool or
			// None equals a string.
			return false, nil
		}
	default:
		return false, typeErrorf("argument of type '%s' is not iterable", typeName(container))
	}
}

// pyStartsWith is Python's a.startswith(b) (or endswith).
func pyStartsWith(a, b any, method string) (bool, error) {
	s, ok := a.(string)
	if !ok {
		return false, attributeErrorf("'%s' object has no attribute '%s'", typeName(a), method)
	}
	affix, ok := b.(string)
	if !ok {
		return false, typeErrorf("%s first arg must be str or a tuple of str, not %s", method, typeName(b))
	}
	if method == "startswith" {
		return strings.HasPrefix(s, affix), nil
	}
	return strings.HasSuffix(s, affix), nil
}

// pyModulo is Python's a % base for a positive int base: the result takes
// the sign of the divisor (floored modulo), for ints and floats alike.
func pyModulo(a any, base int64) (any, error) {
	n, ok := asNumber(a)
	if !ok {
		if _, isString := a.(string); isString {
			return nil, typeErrorf("not all arguments converted during string formatting")
		}
		return nil, typeErrorf("unsupported operand type(s) for %%: '%s' and 'int'", typeName(a))
	}
	if !n.isFloat {
		r := n.i % base
		if r != 0 && (r < 0) != (base < 0) {
			r += base
		}
		return r, nil
	}
	b := float64(base)
	r := math.Mod(n.f, b)
	if r != 0 && (r < 0) != (b < 0) {
		r += b
	}
	return r, nil
}

// pyStr is Python's str(v): strings as they are, anything else as repr.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyRepr is Python's repr(v) for JSON values.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyFloatRepr(x)
	case string:
		return pyStringRepr(x)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *object:
		parts := make([]string, len(x.keys))
		for i, key := range x.keys {
			parts[i] = pyStringRepr(key) + ": " + pyRepr(x.values[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}

// pyFloatRepr formats f as Python's repr does: the shortest digits that read
// back to f, fixed notation for exponents from -4 to 15, scientific beyond.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText) //nolint:errcheck // FormatFloat always writes a valid exponent
	if exp < -4 || exp >= 16 {
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		return fmt.Sprintf("%se%s%02d", mantissa, sign, exp)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyStringRepr quotes s as Python's repr does.
func pyStringRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\' || r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == ' ' || unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
