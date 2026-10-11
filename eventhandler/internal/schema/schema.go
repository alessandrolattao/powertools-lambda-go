// Package schema validates JSON values against the JSON Schema documents the
// typed operations of the event handler generate from struct tags (through
// github.com/swaggest/jsonschema-go): a deliberately small JSON Schema
// 2020-12 validator.
//
// A general validator compiles the meta-schemas of every draft when its
// package initializes, which costs milliseconds of every cold start; the
// event handler only needs the keywords its reflector writes. Compile
// rejects any other keyword, so that a schema is never half enforced.
//
// Supported: type (with null), enum, const, format (date-time, date, time,
// duration, email, hostname, ipv4, ipv6, uri, uri-reference, iri,
// iri-reference, uuid, regex; int32, int64, float, double, byte, binary and
// password are annotations), $ref to a JSON pointer of the same document,
// properties, required, additionalProperties, minProperties, maxProperties,
// items, minItems, maxItems, uniqueItems, minLength, maxLength (in runes),
// pattern (RE2, unanchored), minimum, maximum, exclusiveMinimum,
// exclusiveMaximum, multipleOf, allOf, anyOf, oneOf, boolean schemas.
// Annotations (title, description, default, examples, deprecated...) and x-
// extensions are ignored.
//
// The order and the shape of the failures follow
// github.com/santhosh-tekuri/jsonschema/v6, which the event handler used
// before: type, const, enum and format stop the evaluation of their schema;
// the other keywords all report.
package schema

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The keywords, as Failure.Keyword names them.
const (
	kwType                 = "type"
	kwEnum                 = "enum"
	kwConst                = "const"
	kwFormat               = "format"
	kwRequired             = "required"
	kwProperties           = "properties"
	kwAdditionalProperties = "additionalProperties"
	kwMinProperties        = "minProperties"
	kwMaxProperties        = "maxProperties"
	kwItems                = "items"
	kwMinItems             = "minItems"
	kwMaxItems             = "maxItems"
	kwUniqueItems          = "uniqueItems"
	kwMinLength            = "minLength"
	kwMaxLength            = "maxLength"
	kwPattern              = "pattern"
	kwMinimum              = "minimum"
	kwMaximum              = "maximum"
	kwExclusiveMinimum     = "exclusiveMinimum"
	kwExclusiveMaximum     = "exclusiveMaximum"
	kwMultipleOf           = "multipleOf"
	kwAllOf                = "allOf"
	kwAnyOf                = "anyOf"
	kwOneOf                = "oneOf"
	kwFalse                = "false"

	typeNull   = "null"
	typeNumber = "number"
)

// ErrUnsupported reports a schema that uses something this validator does
// not implement.
var ErrUnsupported = errors.New("unsupported JSON Schema")

// Schema is a compiled schema. It is safe for concurrent use.
type Schema struct {
	boolean *bool

	types    []string
	enum     []any
	hasEnum  bool
	constant any
	hasConst bool
	format   *format
	ref      *Schema

	minProperties, maxProperties *int
	required                     []string
	properties                   map[string]*Schema
	additional                   *Schema

	minItems, maxItems *int
	uniqueItems        bool
	items              *Schema

	minLength, maxLength *int
	pattern              *regexp.Regexp

	minimum, maximum                   *big.Rat
	exclusiveMinimum, exclusiveMaximum *big.Rat
	multipleOf                         *big.Rat

	allOf, anyOf, oneOf []*Schema
}

// Compiler compiles the schemas of one document, sharing what they
// reference.
type Compiler struct {
	root  any
	cache map[string]*Schema
}

// NewCompiler returns a compiler for the document root, a decoded JSON value
// (from Decode or from encoding/json/v2: numbers as Number or float64).
func NewCompiler(root any) *Compiler {
	return &Compiler{root: root, cache: map[string]*Schema{}}
}

// ErrCycle reports references that lead back to themselves without going
// down the instance, which no value can satisfy or fail.
var ErrCycle = fmt.Errorf("%w: reference cycle", ErrUnsupported)

// Compile compiles the schema at pointer, a URI fragment such as
// "#/$defs/request" ("#" is the whole document).
func (c *Compiler) Compile(pointer string) (*Schema, error) {
	s, err := c.compile(pointer)
	if err != nil {
		return nil, err
	}
	if err := checkCycles(s); err != nil {
		return nil, fmt.Errorf("schema %s: %w", pointer, err)
	}
	return s, nil
}

// checkCycles looks for a loop among the subschemas that apply to the same
// value ($ref, allOf, anyOf, oneOf): validating it would recurse forever.
// Loops through properties and items go down the instance, and end.
func checkCycles(root *Schema) error {
	var all []*Schema
	seen := map[*Schema]bool{}
	queue := []*Schema{root}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if seen[s] {
			continue
		}
		seen[s] = true
		all = append(all, s)
		queue = append(queue, s.sameValue()...)
		queue = append(queue, s.children()...)
	}
	const visiting, done = 1, 2
	state := map[*Schema]int{}
	var visit func(s *Schema) error
	visit = func(s *Schema) error {
		switch state[s] {
		case visiting:
			return ErrCycle
		case done:
			return nil
		}
		state[s] = visiting
		for _, sub := range s.sameValue() {
			if err := visit(sub); err != nil {
				return err
			}
		}
		state[s] = done
		return nil
	}
	for _, s := range all {
		if err := visit(s); err != nil {
			return err
		}
	}
	return nil
}

// sameValue are the subschemas that apply to the value itself.
func (s *Schema) sameValue() []*Schema {
	out := slices.Concat(s.allOf, s.anyOf, s.oneOf)
	if s.ref != nil {
		out = append(out, s.ref)
	}
	return out
}

// children are the subschemas that apply to values inside this one.
func (s *Schema) children() []*Schema {
	var out []*Schema
	for _, name := range sortedKeys(s.properties) {
		out = append(out, s.properties[name])
	}
	if s.additional != nil {
		out = append(out, s.additional)
	}
	if s.items != nil {
		out = append(out, s.items)
	}
	return out
}

func (c *Compiler) compile(pointer string) (*Schema, error) {
	if s, ok := c.cache[pointer]; ok {
		return s, nil
	}
	node, err := c.resolve(pointer)
	if err != nil {
		return nil, err
	}
	s := &Schema{}
	// Cached before its children compile, so that a recursive reference
	// finds it.
	c.cache[pointer] = s
	if err := c.fill(s, node, pointer); err != nil {
		delete(c.cache, pointer)
		return nil, err
	}
	return s, nil
}

// resolve follows a JSON pointer fragment from the root.
func (c *Compiler) resolve(pointer string) (any, error) {
	frag, ok := strings.CutPrefix(pointer, "#")
	if !ok {
		return nil, fmt.Errorf("%w: reference %q is not a pointer into the document", ErrUnsupported, pointer)
	}
	frag, err := url.PathUnescape(frag)
	if err != nil {
		return nil, fmt.Errorf("%w: reference %q: %w", ErrUnsupported, pointer, err)
	}
	node := c.root
	if frag == "" {
		return node, nil
	}
	if !strings.HasPrefix(frag, "/") {
		return nil, fmt.Errorf("%w: reference %q is not a JSON pointer", ErrUnsupported, pointer)
	}
	for tok := range strings.SplitSeq(frag[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch n := node.(type) {
		case map[string]any:
			child, ok := n[tok]
			if !ok {
				return nil, fmt.Errorf("%w: reference %q: no member %q", ErrUnsupported, pointer, tok)
			}
			node = child
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(n) {
				return nil, fmt.Errorf("%w: reference %q: no item %q", ErrUnsupported, pointer, tok)
			}
			node = n[i]
		default:
			return nil, fmt.Errorf("%w: reference %q goes through a %T", ErrUnsupported, pointer, node)
		}
	}
	return node, nil
}

// annotations are the keywords that do not assert anything.
var annotations = map[string]bool{
	"$schema": true, "$comment": true, "$defs": true, "definitions": true,
	"title": true, "description": true, "default": true, "examples": true, "example": true,
	"deprecated": true, "readOnly": true, "writeOnly": true,
	"contentMediaType": true, "contentEncoding": true,
	// OpenAPI's own vocabulary.
	"discriminator": true, "xml": true, "externalDocs": true,
}

func (c *Compiler) fill(s *Schema, node any, ptr string) error {
	if b, ok := node.(bool); ok {
		s.boolean = &b
		return nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: schema %s is a %T", ErrUnsupported, ptr, node)
	}
	for _, key := range sortedKeys(m) {
		if err := c.keyword(s, key, m[key], ptr); err != nil {
			return fmt.Errorf("schema %s: %w", ptr, err)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// keyword compiles one keyword of a schema object.
func (c *Compiler) keyword(s *Schema, key string, v any, ptr string) error {
	if annotations[key] || strings.HasPrefix(key, "x-") {
		return nil
	}
	switch key {
	case kwType, kwEnum, kwConst, kwFormat:
		return c.general(s, key, v)
	case "$ref":
		ref, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: $ref is a %T", ErrUnsupported, v)
		}
		target, err := c.compile(ref)
		s.ref = target
		return err
	case kwProperties, kwRequired, kwAdditionalProperties, kwMinProperties, kwMaxProperties:
		return c.object(s, key, v, ptr)
	case kwItems, kwMinItems, kwMaxItems, kwUniqueItems:
		return c.array(s, key, v, ptr)
	case kwMinLength, kwMaxLength, kwPattern:
		return compileString(s, key, v)
	case kwMinimum, kwMaximum, kwExclusiveMinimum, kwExclusiveMaximum, kwMultipleOf:
		return compileNumber(s, key, v)
	case kwAllOf, kwAnyOf, kwOneOf:
		return c.combinator(s, key, v, ptr)
	default:
		return fmt.Errorf("%w: keyword %q", ErrUnsupported, key)
	}
}

func (c *Compiler) general(s *Schema, key string, v any) error {
	switch key {
	case kwType:
		types, err := compileTypes(v)
		s.types = types
		return err
	case kwEnum:
		values, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%w: enum is a %T", ErrUnsupported, v)
		}
		s.hasEnum = true
		for _, item := range values {
			s.enum = append(s.enum, normalize(item))
		}
		return nil
	case kwConst:
		s.hasConst, s.constant = true, normalize(v)
		return nil
	default:
		name, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: format is a %T", ErrUnsupported, v)
		}
		f, err := lookupFormat(name)
		s.format = f
		return err
	}
}

// jsonTypes are the JSON Schema types, in the order failures list them.
var jsonTypes = []string{typeNull, "boolean", typeNumber, "integer", "string", "array", "object"}

func compileTypes(v any) ([]string, error) {
	var names []string
	switch t := v.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, item := range t {
			name, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%w: type item is a %T", ErrUnsupported, item)
			}
			names = append(names, name)
		}
	default:
		return nil, fmt.Errorf("%w: type is a %T", ErrUnsupported, v)
	}
	for _, name := range names {
		if !slices.Contains(jsonTypes, name) {
			return nil, fmt.Errorf("%w: type %q", ErrUnsupported, name)
		}
	}
	// A set, in the canonical order.
	return slices.DeleteFunc(slices.Clone(jsonTypes), func(t string) bool { return !slices.Contains(names, t) }), nil
}

func (c *Compiler) object(s *Schema, key string, v any, ptr string) error {
	var err error
	switch key {
	case kwProperties:
		props, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: properties is a %T", ErrUnsupported, v)
		}
		s.properties = make(map[string]*Schema, len(props))
		for _, name := range sortedKeys(props) {
			if s.properties[name], err = c.compile(ptr + "/properties/" + escape(name)); err != nil {
				return err
			}
		}
	case kwRequired:
		names, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%w: required is a %T", ErrUnsupported, v)
		}
		for _, n := range names {
			name, ok := n.(string)
			if !ok {
				return fmt.Errorf("%w: required item is a %T", ErrUnsupported, n)
			}
			s.required = append(s.required, name)
		}
	case kwAdditionalProperties:
		s.additional, err = c.compile(ptr + "/" + kwAdditionalProperties)
	case kwMinProperties:
		s.minProperties, err = count(key, v)
	default:
		s.maxProperties, err = count(key, v)
	}
	return err
}

func (c *Compiler) array(s *Schema, key string, v any, ptr string) error {
	var err error
	switch key {
	case kwItems:
		if _, tuple := v.([]any); tuple {
			return fmt.Errorf("%w: items as an array", ErrUnsupported)
		}
		s.items, err = c.compile(ptr + "/" + kwItems)
	case kwMinItems:
		s.minItems, err = count(key, v)
	case kwMaxItems:
		s.maxItems, err = count(key, v)
	default:
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("%w: uniqueItems is a %T", ErrUnsupported, v)
		}
		s.uniqueItems = b
	}
	return err
}

func compileString(s *Schema, key string, v any) error {
	var err error
	switch key {
	case kwMinLength:
		s.minLength, err = count(key, v)
	case kwMaxLength:
		s.maxLength, err = count(key, v)
	default:
		expr, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: pattern is a %T", ErrUnsupported, v)
		}
		pattern, perr := regexp.Compile(expr)
		if perr != nil {
			return fmt.Errorf("%w: pattern %q: %w", ErrUnsupported, expr, perr)
		}
		s.pattern = pattern
	}
	return err
}

func compileNumber(s *Schema, key string, v any) error {
	r, ok := rat(normalize(v))
	if !ok {
		return fmt.Errorf("%w: %s is a %T", ErrUnsupported, key, v)
	}
	switch key {
	case kwMinimum:
		s.minimum = r
	case kwMaximum:
		s.maximum = r
	case kwExclusiveMinimum:
		s.exclusiveMinimum = r
	case kwExclusiveMaximum:
		s.exclusiveMaximum = r
	default:
		if r.Sign() <= 0 {
			return fmt.Errorf("%w: multipleOf %s is not positive", ErrUnsupported, r.RatString())
		}
		s.multipleOf = r
	}
	return nil
}

func (c *Compiler) combinator(s *Schema, key string, v any, ptr string) error {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return fmt.Errorf("%w: %s must be a non-empty array", ErrUnsupported, key)
	}
	schemas := make([]*Schema, len(items))
	for i := range items {
		sub, err := c.compile(ptr + "/" + key + "/" + strconv.Itoa(i))
		if err != nil {
			return err
		}
		schemas[i] = sub
	}
	switch key {
	case kwAllOf:
		s.allOf = schemas
	case kwAnyOf:
		s.anyOf = schemas
	default:
		s.oneOf = schemas
	}
	return nil
}

// count reads a non-negative integer keyword.
func count(key string, v any) (*int, error) {
	r, ok := rat(normalize(v))
	if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
		return nil, fmt.Errorf("%w: %s must be a non-negative integer", ErrUnsupported, key)
	}
	n := int(r.Num().Int64())
	return &n, nil
}

// escape escapes a name for a JSON pointer inside a URI fragment.
func escape(name string) string {
	name = strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
	return url.PathEscape(name)
}
