package datamasking

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The expected values below were produced by CPython 3.14 (repr, str and
// re.sub), the behavior Powertools masks with.

func TestPyStr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want string
	}{
		{in: "plain", want: "plain"},
		{in: nil, want: "None"},
		{in: true, want: "True"},
		{in: false, want: "False"},
		{in: int64(12345), want: "12345"},
		{in: int64(-7), want: "-7"},
		{in: 1.5, want: "1.5"},
		{in: 1e20, want: "1e+20"},
		{in: 30.0, want: "30.0"},
		{in: 1e16, want: "1e+16"},
		{in: 1e15, want: "1000000000000000.0"},
		{in: 0.0001, want: "0.0001"},
		{in: 0.00001, want: "1e-05"},
		{in: negativeZero(), want: "-0.0"},
		{in: 123.456, want: "123.456"},
		{in: 1.5e-7, want: "1.5e-07"},
		{in: 2.5e300, want: "2.5e+300"},
		{in: 1.0 / 3, want: "0.3333333333333333"},
		{in: 100.0, want: "100.0"},
		{in: 5e-324, want: "5e-324"},
		{in: 1.7976931348623157e308, want: "1.7976931348623157e+308"},
		{in: 12345678901234567.0, want: "1.2345678901234568e+16"},
		{in: []any{"Pat", "powertools"}, want: "['Pat', 'powertools']"},
		{in: map[string]any{"k": "v w"}, want: "{'k': 'v w'}"},
		{in: map[string]any{"b": int64(1), "a": []any{nil, true, 1.0}}, want: "{'a': [None, True, 1.0], 'b': 1}"},
		{in: []any{}, want: "[]"},
		{in: map[string]any{}, want: "{}"},
		{in: []any{"it's"}, want: `["it's"]`},
		{in: []any{`a"b`}, want: `['a"b']`},
		{in: []any{`both'"`}, want: `['both\'"']`},
		{in: []any{"\x00\x7f\u0085\u200b\U0001F600é\\"}, want: "['\\x00\\x7f\\x85\\u200b\U0001F600é\\\\']"},
		{in: []any{"tab\tnl\n\r"}, want: `['tab\tnl\n\r']`},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, pyStr(tc.in), "%#v", tc.in)
	}
}

func negativeZero() float64 {
	zero := 0.0
	return -zero
}

func TestRegexMask(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, format, in, want string
	}{
		{pattern: `(.)(.*)(@.*)`, format: `\1****\3`, in: "johndoe@example.com", want: "j****@example.com"},
		{pattern: `(?P<user>[^@]+)@(?P<domain>.+)`, format: `\g<user>@***`, in: "a@b.com", want: "a@***"},
		{pattern: `(\d)`, format: `<\g<1>>`, in: "a1b22", want: "a<1>b<2><2>"},
		{pattern: `(\d)`, format: `\g<0>\g<0>`, in: "x7", want: "x77"},
		{pattern: `(a)|(b)`, format: `[\1\2]`, in: "ab", want: "[a][b]"},
		{pattern: `x`, format: `\n\t\\`, in: "x", want: "\n\t\\"},
		{pattern: `x`, format: `\101`, in: "x", want: "A"},
		{pattern: `x`, format: `\0`, in: "x", want: "\x00"},
		{pattern: `x`, format: `\012`, in: "x", want: "\n"},
		{pattern: `x`, format: `\.\&\é`, in: "x", want: `\.\&\é`},
		{pattern: `x`, format: `$1 $$`, in: "x", want: "$1 $$"},
		{pattern: `(a)(b)(c)(d)(e)(f)(g)(h)(i)(j)(k)`, format: `\11-\10`, in: "abcdefghijk", want: "k-j"},
		{pattern: `\d+`, format: "#", in: "a12b345", want: "a#b#"},
		{pattern: `(?i)DOE`, format: "***", in: "John Doe", want: "John ***"},
		{pattern: `\b[A-Z][a-z]+ [A-Z][a-z]+\b`, format: "[name]", in: "Hello! My name is John Doe", want: "Hello! My name is [name]"},
		{pattern: `z`, format: "y", in: "no match", want: "no match"},
	}
	for _, tc := range cases {
		m, err := newRegexMask(tc.pattern, tc.format)
		require.NoError(t, err, tc.format)
		assert.Equal(t, tc.want, m.replace(tc.in), tc.format)
	}
}

func TestRegexMaskErrors(t *testing.T) {
	t.Parallel()
	cases := []struct{ pattern, format string }{
		{pattern: `[`, format: "x"},
		{pattern: `x`, format: `\q`},
		{pattern: `(x)`, format: `\2`},
		{pattern: `(x)`, format: `\g<name>`},
		{pattern: `(x)`, format: `\g<1`},
		{pattern: `(x)`, format: `\g1`},
		{pattern: `(x)`, format: `\g<>`},
		{pattern: `x`, format: `\`},
		{pattern: `x`, format: `\400`},
		{pattern: `(?<=a)b`, format: "x"}, // lookbehind: Python only
	}
	for _, tc := range cases {
		_, err := newRegexMask(tc.pattern, tc.format)
		require.ErrorIs(t, err, ErrInvalidRegex, "%s %s", tc.pattern, tc.format)
	}
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()
	got, err := decodeJSON([]byte(`{"i":30,"f":30.0,"e":1e2,"big":123456789012345678901,"n":-0,"dup":1,"dup":2,"s":"x","a":[true,null]}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"i": int64(30), "f": 30.0, "e": 100.0, "big": 123456789012345678901.0, "n": int64(0),
		"dup": int64(2), "s": "x", "a": []any{true, nil},
	}, got)

	for _, bad := range []string{``, `{`, `{"a":1} x`, `[1,]`, `{"a" 1}`, "\"\xff\"", `nul`} {
		_, err := decodeJSON([]byte(bad))
		require.ErrorIs(t, err, ErrUnsupportedType, bad)
	}
}

// selects evaluates an expression the way Erase finds what to mask.
func selects(t *testing.T, path string, data any) []any {
	t.Helper()
	compiled, err := compilePath(path)
	require.NoError(t, err, path)
	if compiled.expr == nil {
		return []any{data}
	}
	return compiled.expr.Get(data)
}

func TestPathDialect(t *testing.T) {
	t.Parallel()
	data, err := decodeJSON([]byte(`{
		"name": "Carlos", "card_number": "1111", "non-existing": "h", "1": "one", "-1": "minus", "a b": "space",
		"it's": "quote", "@at": "at", "$": "dollar",
		"address": [
			{"postcode": 12345, "street": "a", "tags": ["x"]},
			{"postcode": 67890, "street": "b", "kind": "home"},
			{"postcode": 11111, "street": "c", "kind": "work", "active": true},
			{"postcode": 12000.5, "street": "d"}
		],
		"category": {"sub": {"name": "deep"}},
		"a": {"1": {"None": "hello"}, "b": {"3": {"4": "goodbye"}}},
		"3": {"True": "t"}
	}`))
	require.NoError(t, err)

	cases := []struct {
		path string
		want []any
	}{
		// The expressions of the Powertools documentation and tests.
		{path: "card_number", want: []any{"1111"}},
		{path: "$.card_number", want: []any{"1111"}},
		{path: "category.sub.name", want: []any{"deep"}},
		{path: "category..name", want: []any{"deep"}},
		{path: "address[*].street", want: []any{"a", "b", "c", "d"}},
		{path: "address[1].street", want: []any{"b"}},
		{path: "address[-1].street", want: []any{"d"}},
		{path: "$.address[?(@.postcode > 12000)].street", want: []any{"a", "b", "d"}},
		{path: "a.'1'.None", want: []any{"hello"}},
		{path: "a..'4'", want: []any{"goodbye"}},
		{path: "'3'..True", want: []any{"t"}},
		{path: "non-existing", want: []any{"h"}},
		// The rest of the accepted dialect.
		{path: `a."1".None`, want: []any{"hello"}},
		{path: `$.a.1.None`, want: []any{"hello"}},
		{path: `-1`, want: []any{"minus"}},
		{path: `'a b'`, want: []any{"space"}},
		{path: `'it\'s'`, want: []any{"quote"}},
		{path: `"it's"`, want: []any{"quote"}},
		{path: `'@at'`, want: []any{"at"}},
		{path: `'$'`, want: []any{"dollar"}},
		{path: `$['card_number']`, want: []any{"1111"}},
		{path: `$[card_number]`, want: []any{"1111"}},
		{path: `$.[card_number]`, want: []any{"1111"}},
		{path: `[card_number]`, want: []any{"1111"}},
		{path: `address[0,2].street`, want: []any{"a", "c"}},
		{path: `address[1:3].street`, want: []any{"b", "c"}},
		{path: `address[:1].street`, want: []any{"a"}},
		{path: `address[-2:].street`, want: []any{"c", "d"}},
		{path: `address[::2].street`, want: []any{"a", "c"}},
		{path: `address.[0].street`, want: []any{"a"}},
		{path: ` $ . address [ 0 ] . street `, want: []any{"a"}},
		{path: `address..tags[0]`, want: []any{"x"}},
		{path: `address[?(@.postcode < 12000)].street`, want: []any{"c"}},
		{path: `address[?(@.postcode >= 67890)].street`, want: []any{"b"}},
		{path: `address[?(@.postcode <= 11111)].street`, want: []any{"c"}},
		{path: `address[?(@.postcode == 12345)].street`, want: []any{"a"}},
		{path: `address[?(@.postcode = 12345)].street`, want: []any{"a"}},
		{path: `address[?(@.postcode > 12000.25)].street`, want: []any{"a", "b", "d"}},
		{path: `address[?(@.kind == 'home')].street`, want: []any{"b"}},
		{path: `address[?(@.kind == "home")].street`, want: []any{"b"}},
		{path: `address[?(@.kind == home)].street`, want: []any{"b"}},
		{path: `address[?(@.kind != home)].street`, want: []any{"a", "c", "d"}},
		{path: `address[?(@.kind =~ 'w.r')].street`, want: []any{"c"}},
		{path: `address[?(@.active == true)].street`, want: []any{"c"}},
		{path: `address[?(@.kind)].street`, want: []any{"b", "c"}},
		{path: `address[?kind].street`, want: []any{"b", "c"}},
		{path: `address[?(!@.kind)].street`, want: []any{"a", "d"}},
		{path: `address[?(@.postcode > 12000 & @.kind)].street`, want: []any{"b"}},
		{path: `address[?(@.postcode > 12000) & (@.kind == home)].street`, want: []any{"b"}},
		{path: `address[?postcode > 12000].street`, want: []any{"a", "b", "d"}},
		{path: `address[?(@.tags[0] == x)].street`, want: []any{"a"}},
		{path: `missing`, want: nil},
		{path: `address[9].street`, want: nil},
	}
	for _, tc := range cases {
		assert.ElementsMatch(t, tc.want, selects(t, tc.path, data), tc.path)
	}
	assert.Len(t, selects(t, "category.*", data), 1)
	assert.Len(t, selects(t, "address[*]", data), 4)
	assert.Len(t, selects(t, "name,card_number", data), 2)
	assert.Len(t, selects(t, "$..street", data), 4)
	assert.Equal(t, []any{data}, selects(t, "$", data))
}

func TestPathRejected(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"", "user.ssn...", "a.", "a..", "a[", "a[0", "a]", "[]", "a[?]", "a[?(@.b > )]", "a[?(@.b > 1]",
		"a | b", "a where b", "a & b", "`this`", "a.`len`", "$.a.$", "a[?(@.b > 1 || @.c)]",
		"a[?(@.b > 1 && @.c)]", "a[?(@.b =~ 5)]", "a[?(@.b =~ '(')]", "a[1:2:0]", "a[99999999999]",
		"'unterminated", `'esc\`, "a[0,]", "a[0,x]", "(a)", "a.b[c", "a b", "a.-", "@", "a+1", "a[?(@.b + 1 > 2)]",
		"a[?(" + strings.Repeat("(", 40) + "@.b" + strings.Repeat(")", 40) + ")]",
	} {
		_, err := compilePath(path)
		require.Error(t, err, path)
		assert.True(t, errors.Is(err, errPathSyntax) || errors.Is(err, errPathUnsupported) || errors.Is(err, errPathTooDeep), path)
	}
}
