// Package schematest generates JSON Schema documents of the subset the
// schema package implements, and JSON instances, from fuzzer bytes, so that
// tests can compare the validator with a reference implementation.
package schematest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
)

// Case generates a schema document and an instance, as JSON, from two
// streams of fuzzer bytes.
func Case(schemaBytes, instanceBytes []byte) (schemaJSON, instanceJSON []byte, err error) {
	sg, ig := &gen{data: schemaBytes}, &gen{data: instanceBytes}
	if schemaJSON, err = json.Marshal(sg.document(), json.Deterministic(true)); err != nil {
		return nil, nil, fmt.Errorf("encode the schema: %w", err)
	}
	if instanceJSON, err = json.Marshal(ig.value(0), json.Deterministic(true)); err != nil {
		return nil, nil, fmt.Errorf("encode the instance: %w", err)
	}
	return schemaJSON, instanceJSON, nil
}

// gen draws choices from the fuzzer's bytes.
type gen struct {
	data []byte
	i    int
}

func (g *gen) next(n int) int {
	if g.i >= len(g.data) || n <= 0 {
		return 0
	}
	b := g.data[g.i]
	g.i++
	return int(b) % n
}

func (g *gen) flag() bool { return g.next(3) == 0 }

var (
	numbers = []string{"0", "1", "-1", "2", "2.5", "1.0", "10", "1e2", "-0.5", "12345678901234567890", "3"}
	strs    = []string{
		"", "a", "ab", "abc", "é", "日本", "x1", "[", "B",
		"2024-01-02", "2024-02-30", "2024-01-02T03:04:05Z", "2024-01-02T03:04:05.5+01:00", "2024-01-02 03:04:05Z",
		"23:59:60Z", "12:00:60Z", "12:00:00+25:00", "P1D", "PT1H30M", "P1W", "PT", "P1H",
		"a@b.co", "a..b@c.d", `"q"@x.org`, "x@[127.0.0.1]", "x@[IPv6:::1]", "no-at",
		"example.com", "-bad.com", "127.0.0.1", "256.0.0.1", "01.0.0.1", "::1", "fe80::1%eth0", "1:2",
		"http://x", "http://[::1]:80/", "http://::1/", "/rel", `a\b`, "mailto:x@y",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "3f2504e0-4f89-11d3-9a0c-0305e82c330", "[a-", "a+",
	}
	keys     = []string{"a", "b", "c", "d"}
	types    = []string{"null", "boolean", "integer", "number", "string", "array", "object"}
	patterns = []string{"^a", "b$", "[0-9]+", "^$", "x|y", "é"}
	formats  = []string{"date-time", "date", "time", "duration", "email", "hostname", "ipv4", "ipv6", "uri", "uri-reference", "iri", "iri-reference", "uuid", "regex", "int64", "double", "binary"}
	bounds   = []string{"-1", "0", "1", "2.5", "10", "3"}
	multiple = []string{"0.5", "1", "2", "3", "0.1"}
)

func raw(s string) jsontext.Value { return jsontext.Value(s) }

// value generates a JSON instance.
func (g *gen) value(depth int) any {
	choice := g.next(8)
	if depth > 2 && choice >= 6 {
		choice = g.next(6)
	}
	switch choice {
	case 0:
		return nil
	case 1:
		return g.flag()
	case 2, 3:
		return raw(numbers[g.next(len(numbers))])
	case 4, 5:
		return strs[g.next(len(strs))]
	case 6:
		n := g.next(4)
		arr := make([]any, n)
		for i := range arr {
			arr[i] = g.value(depth + 1)
		}
		return arr
	default:
		obj := map[string]any{}
		for range g.next(4) {
			obj[keys[g.next(len(keys))]] = g.value(depth + 1)
		}
		return obj
	}
}

// schema generates a schema of the supported subset.
func (g *gen) schema(depth int) any {
	if g.next(12) == 0 {
		return g.flag()
	}
	s := map[string]any{}
	g.typeKeyword(s)
	g.stringKeywords(s)
	g.numberKeywords(s)
	if depth < 3 {
		g.objectKeywords(s, depth)
		g.arrayKeywords(s, depth)
		g.combinators(s, depth)
	}
	g.valueKeywords(s)
	if g.flag() {
		s["description"] = "annotation"
	}
	return s
}

func (g *gen) typeKeyword(s map[string]any) {
	if g.next(4) == 0 {
		return
	}
	t := []any{types[g.next(len(types))]}
	if second := types[g.next(len(types))]; g.flag() && second != t[0] {
		t = append(t, second)
	}
	if len(t) == 1 && g.flag() {
		s["type"] = t[0]
		return
	}
	s["type"] = t
}

func (g *gen) valueKeywords(s map[string]any) {
	if g.next(6) == 0 {
		n := g.next(3) + 1
		enum := make([]any, 0, n)
		for range n {
			enum = append(enum, g.value(2))
		}
		s["enum"] = enum
	}
	if g.next(10) == 0 {
		s["const"] = g.value(2)
	}
	if g.next(8) == 0 {
		s["$ref"] = "#/$defs/d" + strconv.Itoa(g.next(2))
	}
}

func (g *gen) stringKeywords(s map[string]any) {
	if g.next(4) == 0 {
		s["minLength"] = raw(strconv.Itoa(g.next(4)))
	}
	if g.next(4) == 0 {
		s["maxLength"] = raw(strconv.Itoa(g.next(4)))
	}
	if g.next(5) == 0 {
		s["pattern"] = patterns[g.next(len(patterns))]
	}
	if g.next(3) == 0 {
		s["format"] = formats[g.next(len(formats))]
	}
}

func (g *gen) numberKeywords(s map[string]any) {
	for _, kw := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		if g.next(6) == 0 {
			s[kw] = raw(bounds[g.next(len(bounds))])
		}
	}
	if g.next(6) == 0 {
		s["multipleOf"] = raw(multiple[g.next(len(multiple))])
	}
}

func (g *gen) objectKeywords(s map[string]any, depth int) {
	if g.next(3) == 0 {
		props := map[string]any{}
		for range g.next(3) + 1 {
			props[keys[g.next(len(keys))]] = g.schema(depth + 1)
		}
		s["properties"] = props
	}
	if g.next(4) == 0 {
		req := []any{}
		for i := range keys {
			if g.flag() || (i == len(keys)-1 && len(req) == 0) {
				req = append(req, keys[i])
			}
		}
		s["required"] = req
	}
	if g.next(5) == 0 {
		if g.flag() {
			s["additionalProperties"] = false
		} else {
			s["additionalProperties"] = g.schema(depth + 1)
		}
	}
	g.counts(s, "minProperties", "maxProperties")
}

func (g *gen) arrayKeywords(s map[string]any, depth int) {
	g.counts(s, "minItems", "maxItems")
	if g.next(4) == 0 {
		s["items"] = g.schema(depth + 1)
	}
	if g.next(6) == 0 {
		s["uniqueItems"] = g.flag()
	}
}

func (g *gen) counts(s map[string]any, keywords ...string) {
	for _, kw := range keywords {
		if g.next(8) == 0 {
			s[kw] = raw(strconv.Itoa(g.next(3)))
		}
	}
}

func (g *gen) combinators(s map[string]any, depth int) {
	for _, kw := range []string{"allOf", "anyOf", "oneOf"} {
		if g.next(7) != 0 {
			continue
		}
		n := g.next(3) + 1
		branches := make([]any, 0, n)
		for range n {
			branches = append(branches, g.schema(depth+1))
		}
		s[kw] = branches
	}
}

// document generates a schema document with the definitions its
// references point to.
func (g *gen) document() any {
	root := g.schema(0)
	m, ok := root.(map[string]any)
	if !ok {
		return root
	}
	m["$defs"] = map[string]any{"d0": g.schema(2), "d1": g.schema(3)}
	return m
}
