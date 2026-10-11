package schema

import (
	"fmt"
	"math/big"
	"slices"
	"strings"
	"unicode/utf8"
)

// Failure is one way an instance breaks a schema.
type Failure struct {
	// Loc locates the failing value in the instance: member names, and
	// array indexes as ints.
	Loc []any
	// Keyword is the keyword that failed: "type", "required",
	// "minLength"... "false" for a false schema, "anyOf" or "oneOf" when no
	// branch says more.
	Keyword string
	// Names are the missing members of required, or the forbidden ones of
	// additionalProperties.
	Names []string
	// Got and Want are the instance's type and the types allowed, for type.
	Got  string
	Want []string
	// Format is the format that failed.
	Format string
	// Msg explains the failure.
	Msg string
}

// Validate validates v and returns its failures, none when it conforms.
func (s *Schema) Validate(v any) []Failure {
	var out []Failure
	s.validate(v, nil, &out)
	return out
}

func (s *Schema) validate(v any, loc []any, out *[]Failure) {
	if s.boolean != nil {
		if !*s.boolean {
			*out = append(*out, Failure{Loc: loc, Keyword: kwFalse, Msg: "no value is allowed"})
		}
		return
	}
	if f, failed := s.general(v, loc); failed {
		*out = append(*out, f)
		return
	}
	if s.ref != nil {
		s.ref.validate(v, loc, out)
	}
	switch x := v.(type) {
	case map[string]any:
		s.validateObject(x, loc, out)
	case []any:
		s.validateArray(x, loc, out)
	case string:
		s.validateString(x, loc, out)
	default:
		if r, ok := rat(v); ok {
			s.validateNumber(r, loc, out)
		}
	}
	s.validateCombinators(v, loc, out)
}

// general checks the keywords that end the evaluation of the schema when
// they fail: type, const, enum, format.
func (s *Schema) general(v any, loc []any) (Failure, bool) {
	if len(s.types) > 0 && !hasType(s.types, v) {
		got := typeOf(v)
		return Failure{
			Loc: loc, Keyword: kwType, Got: got, Want: s.types,
			Msg: fmt.Sprintf("got %s, want %s", got, strings.Join(s.types, " or ")),
		}, true
	}
	if s.hasConst && !equal(v, s.constant) {
		return Failure{Loc: loc, Keyword: kwConst, Msg: fmt.Sprintf("value must be %v", s.constant)}, true
	}
	if s.hasEnum && !slices.ContainsFunc(s.enum, func(e any) bool { return equal(v, e) }) {
		return Failure{Loc: loc, Keyword: kwEnum, Msg: fmt.Sprintf("value must be one of %v", s.enum)}, true
	}
	if s.format != nil && s.format.check != nil {
		if str, ok := v.(string); ok {
			if err := s.format.check(str); err != nil {
				return Failure{Loc: loc, Keyword: kwFormat, Format: s.format.name, Msg: fmt.Sprintf("not a valid %s: %v", s.format.name, err)}, true
			}
		}
	}
	return Failure{}, false
}

func at(loc []any, elem any) []any {
	return append(slices.Clip(loc), elem)
}

func (s *Schema) validateObject(obj map[string]any, loc []any, out *[]Failure) {
	if s.minProperties != nil && len(obj) < *s.minProperties {
		*out = append(*out, Failure{Loc: loc, Keyword: kwMinProperties, Msg: fmt.Sprintf("minimum %d properties, got %d", *s.minProperties, len(obj))})
	}
	if s.maxProperties != nil && len(obj) > *s.maxProperties {
		*out = append(*out, Failure{Loc: loc, Keyword: kwMaxProperties, Msg: fmt.Sprintf("maximum %d properties, got %d", *s.maxProperties, len(obj))})
	}
	var missing []string
	for _, name := range s.required {
		if _, ok := obj[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		*out = append(*out, Failure{Loc: loc, Keyword: kwRequired, Names: missing, Msg: "missing " + strings.Join(missing, ", ")})
	}
	var forbidden []string
	for _, name := range sortedKeys(obj) {
		if !s.validateMember(name, obj[name], loc, out) {
			forbidden = append(forbidden, name)
		}
	}
	if len(forbidden) > 0 {
		*out = append(*out, Failure{Loc: loc, Keyword: kwAdditionalProperties, Names: forbidden, Msg: "not allowed: " + strings.Join(forbidden, ", ")})
	}
}

// validateMember validates a member with its property schema or the
// additional one, and reports false when additional members are forbidden.
func (s *Schema) validateMember(name string, value any, loc []any, out *[]Failure) bool {
	if prop, ok := s.properties[name]; ok {
		prop.validate(value, at(loc, name), out)
		return true
	}
	switch {
	case s.additional == nil:
		return true
	case s.additional.boolean != nil && !*s.additional.boolean:
		return false
	default:
		s.additional.validate(value, at(loc, name), out)
		return true
	}
}

func (s *Schema) validateArray(arr, loc []any, out *[]Failure) {
	if s.minItems != nil && len(arr) < *s.minItems {
		*out = append(*out, Failure{Loc: loc, Keyword: kwMinItems, Msg: fmt.Sprintf("minimum %d items, got %d", *s.minItems, len(arr))})
	}
	if s.maxItems != nil && len(arr) > *s.maxItems {
		*out = append(*out, Failure{Loc: loc, Keyword: kwMaxItems, Msg: fmt.Sprintf("maximum %d items, got %d", *s.maxItems, len(arr))})
	}
	if s.uniqueItems {
		if i, j, dup := duplicates(arr); dup {
			*out = append(*out, Failure{Loc: loc, Keyword: kwUniqueItems, Msg: fmt.Sprintf("items %d and %d are equal", i, j)})
		}
	}
	if s.items != nil {
		for i, item := range arr {
			s.items.validate(item, at(loc, i), out)
		}
	}
}

func duplicates(arr []any) (first, second int, found bool) {
	for i := 1; i < len(arr); i++ {
		for j := range i {
			if equal(arr[i], arr[j]) {
				return j, i, true
			}
		}
	}
	return -1, -1, false
}

func (s *Schema) validateString(str string, loc []any, out *[]Failure) {
	if s.minLength != nil || s.maxLength != nil {
		n := utf8.RuneCountInString(str)
		if s.minLength != nil && n < *s.minLength {
			*out = append(*out, Failure{Loc: loc, Keyword: kwMinLength, Msg: fmt.Sprintf("minimum length %d, got %d", *s.minLength, n)})
		}
		if s.maxLength != nil && n > *s.maxLength {
			*out = append(*out, Failure{Loc: loc, Keyword: kwMaxLength, Msg: fmt.Sprintf("maximum length %d, got %d", *s.maxLength, n)})
		}
	}
	if s.pattern != nil && !s.pattern.MatchString(str) {
		*out = append(*out, Failure{Loc: loc, Keyword: kwPattern, Msg: fmt.Sprintf("does not match %q", s.pattern.String())})
	}
}

func (s *Schema) validateNumber(r *big.Rat, loc []any, out *[]Failure) {
	check := func(bound *big.Rat, keyword, op string, fails func(cmp int) bool) {
		if bound != nil && fails(r.Cmp(bound)) {
			*out = append(*out, Failure{Loc: loc, Keyword: keyword, Msg: fmt.Sprintf("must be %s %s, got %s", op, bound.RatString(), r.RatString())})
		}
	}
	check(s.minimum, kwMinimum, ">=", func(c int) bool { return c < 0 })
	check(s.maximum, kwMaximum, "<=", func(c int) bool { return c > 0 })
	check(s.exclusiveMinimum, kwExclusiveMinimum, ">", func(c int) bool { return c <= 0 })
	check(s.exclusiveMaximum, kwExclusiveMaximum, "<", func(c int) bool { return c >= 0 })
	if s.multipleOf != nil && !new(big.Rat).Quo(r, s.multipleOf).IsInt() {
		*out = append(*out, Failure{Loc: loc, Keyword: kwMultipleOf, Msg: fmt.Sprintf("must be a multiple of %s, got %s", s.multipleOf.RatString(), r.RatString())})
	}
}

func (s *Schema) validateCombinators(v any, loc []any, out *[]Failure) {
	for _, sub := range s.allOf {
		sub.validate(v, loc, out)
	}
	if len(s.anyOf) > 0 {
		var branches [][]Failure
		for _, sub := range s.anyOf {
			failures := sub.Validate(v)
			if len(failures) == 0 {
				s.validateOneOf(v, loc, out)
				return
			}
			branches = append(branches, failures)
		}
		*out = append(*out, unmatched(branches, loc, kwAnyOf)...)
	}
	s.validateOneOf(v, loc, out)
}

func (s *Schema) validateOneOf(v any, loc []any, out *[]Failure) {
	if len(s.oneOf) == 0 {
		return
	}
	matched := -1
	var branches [][]Failure
	for i, sub := range s.oneOf {
		failures := sub.Validate(v)
		switch {
		case len(failures) > 0:
			branches = append(branches, failures)
		case matched == -1:
			matched = i
		default:
			*out = append(*out, Failure{Loc: loc, Keyword: kwOneOf, Msg: fmt.Sprintf("valid against branches %d and %d", matched, i)})
			return
		}
	}
	if matched == -1 {
		*out = append(*out, unmatched(branches, loc, kwOneOf)...)
	}
}

// unmatched reports the failures of the branches of an anyOf or oneOf none
// of which matched. A branch that failed only because the value is not null
// (the nullable side of an optional value) says nothing about what is
// wrong: it is left out, and when nothing else is left the combinator
// fails as a whole.
func unmatched(branches [][]Failure, loc []any, keyword string) []Failure {
	var out []Failure
	for _, failures := range branches {
		if len(failures) == 1 && failures[0].Keyword == kwType && slices.Equal(failures[0].Want, []string{typeNull}) {
			continue
		}
		for i := range failures {
			f := failures[i]
			f.Loc = append(slices.Clip(loc), f.Loc...)
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []Failure{{Loc: loc, Keyword: keyword, Msg: "no branch matches"}}
	}
	return out
}
