package idempotency

import (
	"crypto/md5" //nolint:gosec // G501: the hash under test
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCanonicalJSONMatchesPython checks the canonical encoder against what
// Python prints. Every want and md5 below comes from
//
//	python3 -c 'import json, hashlib; s = json.dumps(json.loads(INPUT), sort_keys=True); print(s, hashlib.md5(s.encode()).hexdigest())'
func TestCanonicalJSONMatchesPython(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input, want, md5 string
	}{
		{`1`, `1`, "c4ca4238a0b923820dcc509a6f75849b"},
		{`"abc"`, `"abc"`, "ebd9f4c7b06cb0aaf5d13d80e49d8b90"},
		{`null`, `null`, "37a6259cc0c1dae299a7866489dff0bd"},
		{`true`, `true`, "b326b5062b2f0e69046810717534cb09"},
		{`false`, `false`, "68934a3e9455fa72420237eb05902327"},
		{`0`, `0`, "cfcd208495d565ef66e7dff9f98764da"},
		{`-0`, `0`, "cfcd208495d565ef66e7dff9f98764da"},
		{`-0.0`, `-0.0`, "e80c77464a0cfaf2c46a5574f6bc76b9"},
		{`0.0`, `0.0`, "30565a8911a6bb487e3745c0ea3c8224"},
		{`1.0`, `1.0`, "e4c2e8edac362acab7123654b9e73432"},
		{`1.5`, `1.5`, "6008647277c4454cecd97d33c069f0ca"},
		{`-1.5`, `-1.5`, "7a09122266225d4e9d9ec42fedbca967"},
		{`10.0`, `10.0`, "43a1437f7f656cd8be7c996c58719e0a"},
		{`1e5`, `100000.0`, "b72a0cf61ebf620d460da6e42fcec084"},
		{`1E5`, `100000.0`, "b72a0cf61ebf620d460da6e42fcec084"},
		{`1e-5`, `1e-05`, "0e6c77caff4e0ac45fc1951e85bbff6a"},
		{`0.0001`, `0.0001`, "ac382d58b1125bb9aeaf1a83ebf5e9c6"},
		{`0.00001`, `1e-05`, "0e6c77caff4e0ac45fc1951e85bbff6a"},
		{`0.00012`, `0.00012`, "6f565c888d996380cc3664830b616582"},
		{`123456789012345.6`, `123456789012345.6`, "d876a9473b6676fe1e3d7d5dd3901f9b"},
		{`1e15`, `1000000000000000.0`, "bc76ddb5ceb03ea84544dbe6c8a5baa6"},
		{`1e16`, `1e+16`, "17ad58a7ab5201decb18b0996a327592"},
		{`1.5e300`, `1.5e+300`, "2139ee3bcd43893ec117ca8c2dd00824"},
		{`-2.5e-300`, `-2.5e-300`, "2af96e1b3b3c3efce186ee78484faecb"},
		{`9007199254740993`, `9007199254740993`, "322d1e02970a2deaaf671adaf4e026c7"},
		{`123456789012345678901234567890`, `123456789012345678901234567890`, "a46857f0ecc21f0a06ea434b94d9cf1d"},
		{`1e400`, `Infinity`, "eb2ac5b04180d8d6011a016aeb8f75b3"},
		{`-1e400`, `-Infinity`, "34ddea809859ac6012054212c3808d1a"},
		{`1e-400`, `0.0`, "30565a8911a6bb487e3745c0ea3c8224"},
		{`0.1`, `0.1`, "cb5ae17636e975f9bf71ddf5bc542075"},
		{`0.3`, `0.3`, "e85b79abfd76b7c13b1334d8d8c194a5"},
		{`2.675`, `2.675`, "c0616bc06587d7f676be016ef56e3d90"},
		{`100.25`, `100.25`, "3c48830f3b5a6ecf40ed73249defff5c"},
		{`9999999999999998.0`, `9999999999999998.0`, "ddde167e9b6164c89cdb14d0ce7013e1"},
		{`3.141592653589793`, `3.141592653589793`, "2836dd7cd23c875da1039ba2b4b4ccd0"},
		{`5e-324`, `5e-324`, "62704aaf46a899ad1563b7f0ced1d768"},
		{`1.7976931348623157e308`, `1.7976931348623157e+308`, "e4749bbf587e511ef5ec505a1800f69a"},
		{`{}`, `{}`, "99914b932bd37a50b983c5e7c90ae93b"},
		{`[]`, `[]`, "d751713988987e9331980363e24189ce"},
		{`{"b":1,"a":2}`, `{"a": 2, "b": 1}`, "5e4d14d82c320bafb2f1286fe486d1f8"},
		{`{"a":[1,2,{"z":null,"y":true}],"b":"x"}`, `{"a": [1, 2, {"y": true, "z": null}], "b": "x"}`, "e14e68a52d7fdbab9cf2c991fb00f236"},
		{`"\u00e9\u4e2d\ud83d\ude00"`, `"\u00e9\u4e2d\ud83d\ude00"`, "53a649243d6bcd577bfb16accd160cfa"},
		{`"tab\tnl\ncr\rbs\bff\f"`, `"tab\tnl\ncr\rbs\bff\f"`, "2e2a237501d369450c5cbcf79d8d8f3d"},
		{`"quote\" back\\ slash/ <>&"`, `"quote\" back\\ slash/ <>&"`, "232de4d8d032374eedca5126c281fbae"},
		{`"\u0000\u001f\u007f\u0080"`, `"\u0000\u001f\u007f\u0080"`, "9d7fccb1edf4ac381140ab553f55d27e"},
		{
			`{"\u00e9":1,"e":2,"z":3,"A":4,"\ud83d\ude00":5,"\uffff":6}`,
			`{"A": 4, "e": 2, "z": 3, "\u00e9": 1, "\uffff": 6, "\ud83d\ude00": 5}`,
			"0a2344009e57140875416654097d8731",
		},
		{
			`{"user_id": 12391, "product_id": "abc", "amount": 10.5}`,
			`{"amount": 10.5, "product_id": "abc", "user_id": 12391}`,
			"13c01b4bcc5e67686c3b62019332dad1",
		},
		{`[null, null]`, `[null, null]`, "99b7882e388402ba516fb344590f7d41"},
		{`{"a": null}`, `{"a": null}`, "542bac9c870e9cd102c3909922945a4d"},
		{`""`, `""`, "9d4568c009d203ab10e33ea9953a0264"},
		{`[[], {}]`, `[[], {}]`, "0850379b107118898aba3055daac8992"},
		{`{"body": "{\"a\": 1}"}`, `{"body": "{\"a\": 1}"}`, "3c071b7b8b064e1ef16e1a39df68b27c"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			tree, err := decodeTree([]byte(tc.input))
			require.NoError(t, err)
			got, err := canonicalJSON(tree)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
			assert.Equal(t, tc.md5, digest(md5.New, got))
		})
	}
}

// TestPythonFloatRepr compares pythonFloat with Python's repr on the values
// testdata/python_float_repr.py generates: random bit patterns over the
// whole range, decimals as payloads carry them, and the boundaries between
// fixed and scientific notation.
func TestPythonFloatRepr(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/python_float_repr.json")
	require.NoError(t, err)
	var vectors [][2]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &vectors))
	require.NotEmpty(t, vectors)
	for _, v := range vectors {
		var bits uint64
		var want string
		require.NoError(t, json.Unmarshal(v[0], &bits))
		require.NoError(t, json.Unmarshal(v[1], &want))
		f := math.Float64frombits(bits)
		assert.Equal(t, want, pythonFloat(f), "repr of %#x", bits)
	}
}

// TestPythonFloat64 covers the numbers JMESPath hands over as float64:
// integral values are integers, as the literals they came from.
func TestPythonFloat64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{-12391, "-12391"},
		{1 << 53, "9007199254740992"},
		{1e18, "1000000000000000000"},
		{1e19, "1e+19"},
		{1.5, "1.5"},
		{0.1, "0.1"},
		{1e-7, "1e-07"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.NaN(), "NaN"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, pythonFloat64(tc.in), "%v", tc.in)
	}
}

func TestCanonicalJSONOfJMESPathValues(t *testing.T) {
	t.Parallel()
	got, err := canonicalJSON(map[string]any{
		"user_id": 12391.0,
		"amount":  10.5,
		"tags":    []any{"a", nil, true},
		"nested":  map[string]any{"\u00e9": "\u00fc"},
	})
	require.NoError(t, err)
	want := `{"amount": 10.5, "nested": {"\u00e9": "\u00fc"}, "tags": ["a", null, true], "user_id": 12391}`
	assert.Equal(t, want, string(got), "byte for byte, not just the same JSON")
}

// TestCanonicalJSONOfOtherValues covers values a custom JMESPath function may
// return: they render as what they marshal to.
func TestCanonicalJSONOfOtherValues(t *testing.T) {
	t.Parallel()
	got, err := canonicalJSON(map[string]any{"n": 42, "s": struct {
		B int    `json:"b"`
		A string `json:"a"`
	}{B: 1, A: "x"}})
	require.NoError(t, err)
	want := `{"n": 42, "s": {"a": "x", "b": 1}}`
	assert.Equal(t, want, string(got))

	_, err = canonicalJSON(map[string]any{"bad": make(chan int)})
	require.Error(t, err)
}

func TestDecodeTreeRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	_, err := decodeTree([]byte(`{"a":`))
	require.Error(t, err)
}

func TestIsMissingKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		data    any
		missing bool
	}{
		{"null", nil, true},
		{"empty string", "", true},
		{"string", "x", false},
		{"zero", 0.0, false},
		{"zero literal", number("0"), false},
		{"false", false, false},
		{"true", true, false},
		{"empty object", map[string]any{}, true},
		{"object of nulls", map[string]any{"a": nil, "b": nil}, true},
		{"object with a value", map[string]any{"a": nil, "b": 0.0}, false},
		{"empty array", []any{}, true},
		{"array of nulls", []any{nil, nil}, true},
		{"array with a value", []any{nil, ""}, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.missing, isMissingKey(tc.data), tc.name)
	}
}

func TestHashFunctionsMatchHashlib(t *testing.T) {
	t.Parallel()
	// python3 -c 'import hashlib; print(hashlib.NAME(b"1").hexdigest())'
	want := map[string]string{
		"md5":      "c4ca4238a0b923820dcc509a6f75849b",
		"sha1":     "356a192b7913b04c54574d18c28d46e6395428ab",
		"sha224":   "e25388fde8290dc286a6164fa2d97e551b53498dcbf7bc378eb1f178",
		"sha256":   "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b",
		"sha384":   "47f05d367b0c32e438fb63e6cf4a5f35c2aa2f90dc7543f8a41a0f95ce8a40a313ab5cf36134a2068c4c969cb50db776",
		"sha512":   "4dff4ea340f0a823f15d3f4f01ab62eae0e5da579ccb851f8db9dfe84c58b2b37b89903a740e1ee172da793a6e79d560e5f7f9bd058a12a280433ed6fa46510a",
		"sha3_224": "300d01f3a910045fefa16d6a149f38167b2503dbc37c1b24fd6f751e",
		"sha3_256": "67b176705b46206614219f47a05aee7ae6a3edbe850bbbe214c536b989aea4d2",
		"sha3_384": "f39de487a8aed2d19069ed7a7bcfc274e9f026bba97c8f059be6a2e5eed051d7ee437b93d80aa6163bf8039543b612dd",
		"sha3_512": "ca2c70bc13298c5109ee0cb342d014906e6365249005fd4beee6f01aee44edb531231e98b50bf6810de6cf687882b09320fdd5f6375d1f2debd966fbf8d03efa",
		"blake2b":  "1ced8f5be2db23a6513eba4d819c73806424748a7bc6fa0d792cc1c7d1775a9778e894aa91413f6eb79ad5ae2f871eafcc78797e4c82af6d1cbfb1a294a10d10",
		"blake2s":  "625851e3876e6e6da405c95ac24687ce4bb2cdd8fbd8459278f6f0ce803e13ee",
	}
	assert.Len(t, hashFunctions, len(want))
	for name, digestWant := range want {
		newHash := hashFunctions[name]
		require.NotNil(t, newHash, name)
		assert.Equal(t, digestWant, digest(newHash, []byte("1")), name)
	}
}
