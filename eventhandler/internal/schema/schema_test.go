package schema_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/schema"
)

func compile(t *testing.T, doc string) *schema.Schema {
	t.Helper()
	root, err := schema.Decode([]byte(doc))
	require.NoError(t, err)
	s, err := schema.NewCompiler(root).Compile("#")
	require.NoError(t, err, doc)
	return s
}

func validate(t *testing.T, s *schema.Schema, instance string) []schema.Failure {
	t.Helper()
	v, err := schema.Decode([]byte(instance))
	require.NoError(t, err)
	return s.Validate(v)
}

func TestCompileRejectsWhatItCannotEnforce(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		`{"not": {}}`,
		`{"if": {}, "then": {}}`,
		`{"patternProperties": {"a": {}}}`,
		`{"prefixItems": [{}]}`,
		`{"items": [{}]}`,
		`{"unevaluatedProperties": false}`,
		`{"dependentRequired": {}}`,
		`{"format": "semver"}`,
		`{"type": "decimal"}`,
		`{"pattern": "(?<=a)"}`,
		`{"minLength": -1}`,
		`{"minLength": 1.5}`,
		`{"multipleOf": 0}`,
		`{"$ref": "https://example.com/schema"}`,
		`{"$ref": "#/$defs/missing"}`,
		`{"$ref": "#/$defs/a", "$defs": {"a": {"$ref": "#/$defs/b"}, "b": {"allOf": [{"$ref": "#/$defs/a"}]}}}`,
		`{"anyOf": []}`,
		`{"enum": "a"}`,
		`{"unknownKeyword": 1}`,
		`12`,
	} {
		root, err := schema.Decode([]byte(doc))
		require.NoError(t, err)
		_, err = schema.NewCompiler(root).Compile("#")
		require.ErrorIs(t, err, schema.ErrUnsupported, doc)
	}
	root, err := schema.Decode([]byte(`{"$ref": "#/$defs/a", "$defs": {"a": {"$ref": "#/$defs/a"}}}`))
	require.NoError(t, err)
	_, err = schema.NewCompiler(root).Compile("#")
	require.ErrorIs(t, err, schema.ErrCycle)
}

func TestAnnotationsAndRecursion(t *testing.T) {
	t.Parallel()
	s := compile(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"title": "Node", "description": "d", "examples": [1], "deprecated": true, "x-owner": "me",
		"type": "object",
		"properties": {
			"name": {"type": "string", "format": "int64", "contentMediaType": "text/plain"},
			"children": {"type": "array", "items": {"$ref": "#"}}
		},
		"required": ["name"]
	}`)
	assert.Empty(t, validate(t, s, `{"name": "a", "children": [{"name": "b", "children": []}]}`))
	failures := validate(t, s, `{"name": "a", "children": [{"children": [{"name": 1}]}]}`)
	require.Len(t, failures, 2)
	assert.Equal(t, []any{"children", 0}, failures[0].Loc)
	assert.Equal(t, "required", failures[0].Keyword)
	assert.Equal(t, []string{"name"}, failures[0].Names)
	assert.Equal(t, []any{"children", 0, "children", 0, "name"}, failures[1].Loc)
	assert.Equal(t, "type", failures[1].Keyword)
	assert.Equal(t, "number", failures[1].Got)
	assert.Equal(t, []string{"string"}, failures[1].Want)
}

func TestKeywords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		schema, instance string
		keywords         []string
	}{
		{`{"type": "integer"}`, `1.0`, nil},
		{`{"type": "integer"}`, `1.5`, []string{"type"}},
		{`{"type": ["string", "null"]}`, `null`, nil},
		{`{"minimum": 0.1}`, `0.1`, nil},
		{`{"multipleOf": 0.1}`, `0.3`, nil},
		{`{"multipleOf": 0.1}`, `0.35`, []string{"multipleOf"}},
		{`{"maximum": 18446744073709551615}`, `18446744073709551616`, []string{"maximum"}},
		{`{"exclusiveMinimum": 1, "exclusiveMaximum": 1}`, `1`, []string{"exclusiveMinimum", "exclusiveMaximum"}},
		{`{"minLength": 2, "maxLength": 2}`, `"éé"`, nil},
		{`{"minLength": 3, "pattern": "^b"}`, `"a"`, []string{"minLength", "pattern"}},
		{`{"enum": [1, "a"]}`, `1.0`, nil},
		{`{"enum": [1, "a"], "minimum": 5}`, `2`, []string{"enum"}},
		{`{"const": {"a": [1]}}`, `{"a": [1.0]}`, nil},
		{`{"uniqueItems": true}`, `[1, 1.0]`, []string{"uniqueItems"}},
		{`{"minItems": 1, "maxItems": 0}`, `[]`, []string{"minItems"}},
		{`{"minProperties": 1, "maxProperties": 0}`, `{"a": 1}`, []string{"maxProperties"}},
		{`{"additionalProperties": false, "properties": {"a": {}}}`, `{"a": 1, "c": 2, "b": 3}`, []string{"additionalProperties"}},
		{`{"additionalProperties": {"type": "string"}}`, `{"a": 1}`, []string{"type"}},
		{`{"format": "email"}`, `12`, nil},
		{`{"format": "email", "minLength": 50}`, `"nope"`, []string{"format"}},
		{`{"allOf": [{"minimum": 2}, {"maximum": 0}]}`, `1`, []string{"minimum", "maximum"}},
		{`{"anyOf": [{"type": "null"}, {"type": "string", "minLength": 3}]}`, `"ab"`, []string{"minLength"}},
		{`{"anyOf": [{"type": "null"}, {"type": "null"}]}`, `1`, []string{"anyOf"}},
		{`{"oneOf": [{"type": "number"}, {"minimum": 0}]}`, `1`, []string{"oneOf"}},
		{`{"oneOf": [{"type": "number"}, {"type": "string"}]}`, `true`, []string{"type", "type"}},
		{`false`, `1`, []string{"false"}},
		{`true`, `1`, nil},
	}
	for _, tc := range tests {
		var got []string
		for _, f := range validate(t, compile(t, tc.schema), tc.instance) {
			got = append(got, f.Keyword)
			assert.NotEmpty(t, f.Msg)
		}
		assert.Equal(t, tc.keywords, got, "%s ~ %s", tc.schema, tc.instance)
	}
}

func TestAdditionalPropertiesNamesAreSorted(t *testing.T) {
	t.Parallel()
	f := validate(t, compile(t, `{"additionalProperties": false}`), `{"c": 1, "a": 2, "b": 3}`)
	require.Len(t, f, 1)
	assert.Equal(t, []string{"a", "b", "c"}, f[0].Names)
}

func TestValidateGoValues(t *testing.T) {
	t.Parallel()
	s := compile(t, `{"type": "object", "properties": {
		"i": {"type": "integer", "minimum": 1}, "u": {"maximum": 2}, "f": {"type": "number"}, "l": {"items": {"type": "boolean"}}
	}}`)
	f := s.Validate(map[string]any{"i": int64(0), "u": uint64(3), "f": float64(1.5), "l": []any{true, "x"}})
	keywords := make([]string, 0, len(f))
	for _, failure := range f {
		keywords = append(keywords, failure.Keyword)
	}
	assert.Equal(t, []string{"minimum", "type", "maximum"}, keywords)
	assert.Equal(t, []any{"l", 1}, f[1].Loc)
}

func TestFormats(t *testing.T) {
	t.Parallel()
	tests := map[string]map[string]bool{
		"date-time":     {"2024-01-02T03:04:05Z": true, "2024-01-02t03:04:05.123+01:00": true, "2024-01-02 03:04:05Z": false, "2024-13-02T03:04:05Z": false, "2016-12-31T23:59:60Z": true, "2016-12-31T22:59:60-01:00": true, "2024-01-02T12:59:60Z": false},
		"date":          {"2024-02-29": true, "2023-02-29": false, "2024-1-02": false},
		"time":          {"03:04:05Z": true, "03:04:05.5+05:30": true, "03:04Z": false, "24:00:00Z": false, "03:04:05+24:00": false, "03:04:05.Z": false, "1::::::::": false},
		"duration":      {"P1D": true, "PT1H30M": true, "P1W": true, "P1Y2M3DT4H5M6S": true, "P": false, "PT": false, "P1H": false, "P1M1Y": false, "P1DT": false, "P1D1W": false, "PT1H1T": false},
		"email":         {"a@b.co": true, `"x y"@b.co`: true, "x@[127.0.0.1]": true, "x@[IPv6:::1]": true, "a..b@c.d": false, ".a@b.c": false, "no-at": false, "a@-b.c": false, "a b@c.d": false},
		"hostname":      {"example.com": true, "example.com.": true, "-a.com": false, "a_b.com": false, "": false},
		"ipv4":          {"127.0.0.1": true, "256.0.0.1": false, "01.0.0.1": false, "1.2.3": false},
		"ipv6":          {"::1": true, "fe80::1%eth0": false, "127.0.0.1": false, "1:2": false},
		"uri":           {"http://x": true, "mailto:a@b": true, "/rel": false, "http://::1/": false, "http://[::1]:80/": true},
		"uri-reference": {"/rel": true, "http://x": true, `a\b`: false, "%zz": false},
		"uuid":          {"3f2504e0-4f89-11d3-9a0c-0305e82c3301": true, "3F2504E0-4F89-11D3-9A0C-0305E82C3301": true, "3f2504e04f8911d39a0c0305e82c3301": false, "3f2504e0-4f89-11d3-9a0c-0305e82c330g": false},
		"regex":         {"a+": true, "(": false},
	}
	for format, cases := range tests {
		s := compile(t, `{"format": "`+format+`"}`)
		for value, valid := range cases {
			instance, err := json.Marshal(value)
			require.NoError(t, err)
			f := validate(t, s, string(instance))
			assert.Equal(t, valid, len(f) == 0, "%s %q: %v", format, value, f)
			if !valid && len(f) == 1 {
				assert.Equal(t, format, f[0].Format)
			}
		}
	}
}

func TestDecode(t *testing.T) {
	t.Parallel()
	v, err := schema.Decode([]byte(`{"a": 1, "a": 12345678901234567890.5, "b": [true, null, "xé"]}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": schema.Number("12345678901234567890.5"), "b": []any{true, nil, "xé"}}, v)

	for _, bad := range []string{"", "{", `{"a":}`, "1 2", "[1,]"} {
		_, derr := schema.Decode([]byte(bad))
		require.Error(t, derr, bad)
	}
	_, err = schema.Decode([]byte("\"\xff\""))
	require.NoError(t, err, "invalid UTF-8 is replaced, as encoding/json does")
}

func TestCompilerSharesDefinitions(t *testing.T) {
	t.Parallel()
	root, err := schema.Decode([]byte(`{"$defs": {
		"a": {"type": "object", "properties": {"t": {"$ref": "#/$defs/shared"}}},
		"b": {"items": {"$ref": "#/$defs/shared"}},
		"shared": {"type": "string"},
		"weird~name/x": {"type": "integer"}
	}}`))
	require.NoError(t, err)
	c := schema.NewCompiler(root)
	a, err := c.Compile("#/$defs/a")
	require.NoError(t, err)
	b, err := c.Compile("#/$defs/b")
	require.NoError(t, err)
	assert.Len(t, a.Validate(map[string]any{"t": int64(1)}), 1)
	assert.Len(t, b.Validate([]any{"ok", true}), 1)
	w, err := c.Compile("#/$defs/weird~0name~1x")
	require.NoError(t, err)
	assert.Len(t, w.Validate("x"), 1)
	_, err = c.Compile("$defs/a")
	require.ErrorIs(t, err, schema.ErrUnsupported)
	_, err = c.Compile("#/$defs/a/properties/t/0")
	require.ErrorIs(t, err, schema.ErrUnsupported)
}
