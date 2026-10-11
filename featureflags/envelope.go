package featureflags

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"sort"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// documentFunction is the private JMESPath function that hands the
// envelope the decoded document. jmespath.Expression.Search would marshal
// and unmarshal a document passed to it, building new maps; the value a
// function returns is used as it is, so the maps the envelope selects are
// the very maps decoded here, whose member order is on record.
const documentFunction = "powertools_feature_flags_document"

// shape is what JMESPath's plain maps and slices forget of a JSON object or
// array: the order of the members, and which numbers were integers (JMESPath
// numbers are float64).
type shape struct {
	keys []string
	ints map[string]int64
}

type arrayShape struct {
	ints map[int]int64
}

// shapes records the shape of every map and non-empty slice handed to
// JMESPath, by identity.
type shapes struct {
	objects map[uintptr]shape
	arrays  map[uintptr]arrayShape
}

// extract applies envelope to the document raw and returns the selection as
// JSON, objects in document order.
func extract(raw []byte, envelope string, functions []jmespath.Function) ([]byte, error) {
	doc, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	s := shapes{objects: make(map[uintptr]shape), arrays: make(map[uintptr]arrayShape)}
	plain := s.toPlain(doc)
	document := jmespath.Function{Name: documentFunction, Call: func([]any) (any, error) { return plain, nil }}
	expr, err := jmespath.Compile(documentFunction+"() | "+envelope, jmespath.WithFunctions(append(slices.Clone(functions), document)...))
	if err != nil {
		return nil, err
	}
	selected, err := expr.Search(nil)
	if err != nil {
		return nil, err
	}
	model, err := s.fromPlain(selected)
	if err != nil {
		return nil, err
	}
	return encodeJSON(model)
}

// toPlain turns a model value into what JMESPath works on, recording the
// shape of each object and array.
func (s *shapes) toPlain(value any) any {
	switch v := value.(type) {
	case int64:
		return float64(v)
	case []any:
		return s.arrayToPlain(v)
	case *object:
		return s.objectToPlain(v)
	default:
		return value
	}
}

func (s *shapes) arrayToPlain(v []any) []any {
	out := make([]any, len(v))
	sh := arrayShape{ints: make(map[int]int64)}
	for i, item := range v {
		if n, ok := item.(int64); ok {
			sh.ints[i] = n
		}
		out[i] = s.toPlain(item)
	}
	if len(out) > 0 {
		s.arrays[reflect.ValueOf(out).Pointer()] = sh
	}
	return out
}

func (s *shapes) objectToPlain(v *object) map[string]any {
	out := make(map[string]any, v.len())
	sh := shape{keys: v.keys, ints: make(map[string]int64)}
	for _, key := range v.keys {
		item := v.values[key]
		if n, ok := item.(int64); ok {
			sh.ints[key] = n
		}
		out[key] = s.toPlain(item)
	}
	s.objects[reflect.ValueOf(out).Pointer()] = sh
	return out
}

// fromPlain turns what JMESPath selected back into a model value. Maps and
// slices of the document get their order and integers back; those the
// expression built itself (a projection, powertools_json) keep their
// floats, and their members come in sorted order.
func (s *shapes) fromPlain(value any) (any, error) {
	switch v := value.(type) {
	case nil, bool, string, float64:
		return v, nil
	case []any:
		return s.arrayFromPlain(v)
	case map[string]any:
		return s.objectFromPlain(v)
	default:
		// A custom function returned another Go type: read it as JSON.
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("envelope result: %w", err)
		}
		return decodeJSON(raw)
	}
}

func (s *shapes) arrayFromPlain(v []any) (any, error) {
	var sh arrayShape
	if len(v) > 0 {
		sh = s.arrays[reflect.ValueOf(v).Pointer()]
	}
	out := make([]any, len(v))
	for i, item := range v {
		if n, ok := sh.ints[i]; ok && item == float64(n) {
			out[i] = n
			continue
		}
		converted, err := s.fromPlain(item)
		if err != nil {
			return nil, err
		}
		out[i] = converted
	}
	return out, nil
}

func (s *shapes) objectFromPlain(v map[string]any) (any, error) {
	sh, known := s.objects[reflect.ValueOf(v).Pointer()]
	keys := sh.keys
	if !known {
		keys = make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
	}
	out := newObject()
	for _, key := range keys {
		item, ok := v[key]
		if !ok {
			continue
		}
		if n, ok := sh.ints[key]; ok && item == float64(n) {
			out.set(key, n)
			continue
		}
		converted, err := s.fromPlain(item)
		if err != nil {
			return nil, err
		}
		out.set(key, converted)
	}
	return out, nil
}
