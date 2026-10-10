package validation

import (
	"maps"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxDefaultsDepth bounds the recursion of applyDefaults, against schemas
// whose references loop.
const maxDefaultsDepth = 256

// applyDefaults fills data, which validated against s, with the defaults
// of the schemas that apply to it, as fastjsonschema's use_default does: a
// property missing from an object, whose schema in "properties" declares a
// "default", is added with a copy of that value; so is a missing position
// of a tuple ("items" as an array, or "prefixItems"). It descends into the
// properties and items present, follows "$ref" and "allOf", and the branch
// of "anyOf", "oneOf" and "if" that the data matches. It returns the
// updated data and reports whether it added anything.
func applyDefaults(s *jsonschema.Schema, data any) (any, bool) {
	var d defaulter
	data = d.apply(s, data, 0)
	return data, d.changed
}

type defaulter struct {
	changed bool
}

func (d *defaulter) apply(s *jsonschema.Schema, v any, depth int) any {
	if s == nil || depth > maxDefaultsDepth {
		return v
	}
	for _, sub := range d.applicable(s, v) {
		v = d.apply(sub, v, depth+1)
	}
	switch value := v.(type) {
	case map[string]any:
		d.object(s, value, depth)
	case []any:
		v = d.array(s, value, depth)
	}
	return v
}

// applicable lists the subschemas that apply to v as a whole.
func (*defaulter) applicable(s *jsonschema.Schema, v any) []*jsonschema.Schema {
	subs := make([]*jsonschema.Schema, 0, 2+len(s.AllOf))
	subs = append(subs, s.Ref, s.RecursiveRef)
	if s.DynamicRef != nil {
		subs = append(subs, s.DynamicRef.Ref)
	}
	subs = append(subs, s.AllOf...)
	subs = append(subs, firstMatch(s.AnyOf, v), firstMatch(s.OneOf, v))
	if s.If != nil {
		if s.If.Validate(v) == nil {
			subs = append(subs, s.Then)
		} else {
			subs = append(subs, s.Else)
		}
	}
	return subs
}

func firstMatch(branches []*jsonschema.Schema, v any) *jsonschema.Schema {
	for _, b := range branches {
		if b.Validate(v) == nil {
			return b
		}
	}
	return nil
}

func (d *defaulter) object(s *jsonschema.Schema, obj map[string]any, depth int) {
	// Present members first, so that a default added below is not
	// descended into: fastjsonschema does not validate the defaults it adds.
	for _, name := range slices.Sorted(maps.Keys(obj)) {
		for _, sub := range memberSchemas(s, name) {
			obj[name] = d.apply(sub, obj[name], depth+1)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		prop := s.Properties[name]
		if _, present := obj[name]; present || prop == nil || prop.Default == nil {
			continue
		}
		obj[name] = clone(*prop.Default)
		d.changed = true
	}
}

// memberSchemas returns the schemas of the member name of an object: its
// "properties" entry and the "patternProperties" it matches, or else
// "additionalProperties".
func memberSchemas(s *jsonschema.Schema, name string) []*jsonschema.Schema {
	var subs []*jsonschema.Schema
	if sub, ok := s.Properties[name]; ok {
		subs = append(subs, sub)
	}
	patterns := slices.SortedFunc(maps.Keys(s.PatternProperties), func(a, b jsonschema.Regexp) int {
		return strings.Compare(a.String(), b.String())
	})
	for _, re := range patterns {
		if re.MatchString(name) {
			subs = append(subs, s.PatternProperties[re])
		}
	}
	if len(subs) > 0 {
		return subs
	}
	if sub, ok := s.AdditionalProperties.(*jsonschema.Schema); ok {
		return []*jsonschema.Schema{sub}
	}
	return nil
}

func (d *defaulter) array(s *jsonschema.Schema, arr []any, depth int) []any {
	tuple := s.PrefixItems
	rest := s.Items2020
	switch items := s.Items.(type) {
	case []*jsonschema.Schema:
		tuple = items
	case *jsonschema.Schema:
		rest = items
	}
	for i, item := range tuple {
		if i < len(arr) {
			arr[i] = d.apply(item, arr[i], depth+1)
			continue
		}
		// Unlike fastjsonschema, stop at a missing position without a
		// default, rather than append the next default at the wrong index.
		if item == nil || item.Default == nil {
			break
		}
		arr = append(arr, clone(*item.Default))
		d.changed = true
	}
	if rest != nil {
		for i := len(tuple); i < len(arr); i++ {
			arr[i] = d.apply(rest, arr[i], depth+1)
		}
	}
	return arr
}

// clone copies a default, so that a handler changing the validated data
// never changes the schema.
func clone(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, e := range value {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, e := range value {
			out[i] = clone(e)
		}
		return out
	default:
		return v
	}
}

// dropFormats turns off the format checks of every schema reachable from
// s, for WithFormatAssertion(false): draft-07 and earlier always assert
// formats in the engine.
func dropFormats(s *jsonschema.Schema) {
	visit(s, map[*jsonschema.Schema]bool{}, func(s *jsonschema.Schema) { s.Format = nil })
}

// visit calls fn on s and on every schema reachable from it, once.
func visit(s *jsonschema.Schema, seen map[*jsonschema.Schema]bool, fn func(*jsonschema.Schema)) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true
	fn(s)
	for _, sub := range children(s) {
		visit(sub, seen, fn)
	}
}

// children lists the subschemas of s.
func children(s *jsonschema.Schema) []*jsonschema.Schema {
	out := []*jsonschema.Schema{
		s.Ref, s.RecursiveRef, s.Not, s.If, s.Then, s.Else, s.PropertyNames,
		s.UnevaluatedProperties, s.Contains, s.Items2020, s.UnevaluatedItems, s.ContentSchema,
	}
	if s.DynamicRef != nil {
		out = append(out, s.DynamicRef.Ref)
	}
	out = append(out, s.AllOf...)
	out = append(out, s.AnyOf...)
	out = append(out, s.OneOf...)
	out = append(out, s.PrefixItems...)
	out = slices.AppendSeq(out, maps.Values(s.Properties))
	out = slices.AppendSeq(out, maps.Values(s.DependentSchemas))
	out = append(out, patternSchemas(s.PatternProperties)...)
	for _, dep := range s.Dependencies {
		if sub, ok := dep.(*jsonschema.Schema); ok {
			out = append(out, sub)
		}
	}
	for _, v := range []any{s.AdditionalProperties, s.AdditionalItems, s.Items} {
		switch sub := v.(type) {
		case *jsonschema.Schema:
			out = append(out, sub)
		case []*jsonschema.Schema:
			out = append(out, sub...)
		}
	}
	return out
}

func patternSchemas(m map[jsonschema.Regexp]*jsonschema.Schema) []*jsonschema.Schema {
	return slices.Collect(maps.Values(m))
}
