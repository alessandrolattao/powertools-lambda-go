package pyjson

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendFloat(t *testing.T) {
	t.Parallel()
	// The expectations are Python's repr of the same floats.
	tests := []struct {
		in   float64
		bits int
		want string
	}{
		{1, 64, "1.0"},
		{0, 64, "0.0"},
		{math.Copysign(0, -1), 64, "-0.0"},
		{0.1, 64, "0.1"},
		{1.0 / 3, 64, "0.3333333333333333"},
		{1e16, 64, "1e+16"},
		{1e15, 64, "1000000000000000.0"},
		{123456789012345678, 64, "1.2345678901234568e+17"},
		{1e-5, 64, "1e-05"},
		{1.5e-7, 64, "1.5e-07"},
		{0.0001, 64, "0.0001"},
		{1.5e300, 64, "1.5e+300"},
		{-2.5, 64, "-2.5"},
		{math.MaxFloat64, 64, "1.7976931348623157e+308"},
		{5e-324, 64, "5e-324"},
		{float64(float32(0.1)), 32, "0.1"},
		{2, 32, "2.0"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, string(AppendFloat(nil, tt.in, tt.bits)), "%v", tt.in)
	}
}

func TestFloat(t *testing.T) {
	t.Parallel()
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := Float(f, 64)
		require.ErrorIs(t, err, ErrNotFinite)
	}
}

func TestOptions(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(map[string]any{
		"b": 1.0, "a": []any{1, float32(2), 2.5, nil, "é"}, "c": map[string]float64{"z": 3, "y": 1e20},
	}, Options())
	require.NoError(t, err)
	// Byte for byte: 1.0 and 1 are the same JSON number, not the same output.
	want := []byte(`{"a":[1,2.0,2.5,null,"é"],"b":1.0,"c":{"y":1e+20,"z":3.0}}`)
	assert.Equal(t, want, got)
	_, err = json.Marshal(math.NaN(), Options())
	require.ErrorIs(t, err, ErrNotFinite)
	_, err = json.Marshal(float32(math.Inf(1)), Options())
	require.ErrorIs(t, err, ErrNotFinite)
}

func TestEscapeNonASCII(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`{"a":"plain"}`:              `{"a":"plain"}`,
		"{\"\u00e9\":\"caf\u00e9\"}": `{"\u00e9":"caf\u00e9"}`,
		"[\"\U0001F600\"]":           `["\ud83d\ude00"]`,
		"[\"del\x7f\"]":              `["del\u007f"]`,
		"[\"\u2028\ufffd\"]":         `["\u2028\ufffd"]`,
	}
	for in, want := range tests {
		assert.Equal(t, want, string(EscapeNonASCII([]byte(in))))
	}
}

// FuzzAppendFloat checks that every finite float reads back the same, in
// the notation Python's repr picks.
func FuzzAppendFloat(f *testing.F) {
	for _, seed := range []float64{0, 1, -1, 0.1, 1e16, 1e15, 1e-5, 1e-4, 1.5e300, 5e-324, math.MaxFloat64} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in float64) {
		if math.IsNaN(in) || math.IsInf(in, 0) {
			return
		}
		out := string(AppendFloat(nil, in, 64))
		back, err := strconv.ParseFloat(out, 64)
		require.NoError(t, err)
		require.Equal(t, math.Float64bits(in), math.Float64bits(back), out)
		require.True(t, strings.ContainsAny(out, ".e"), out)
		exp := strings.Contains(out, "e")
		if abs := math.Abs(in); abs != 0 {
			wantExp := abs < 1e-4 || abs >= 1e16
			require.Equal(t, wantExp, exp, out)
		}
	})
}

// FuzzEscapeNonASCII checks that the escaped document is ASCII and means the
// same string.
func FuzzEscapeNonASCII(f *testing.F) {
	for _, seed := range []string{"", "plain", "café", "😀", "\x7f", "\xff", "\u2028", "tab\there"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		quoted, err := jsontext.AppendQuote(nil, in)
		if err != nil {
			require.False(t, utf8.ValidString(in))
		}
		escaped := EscapeNonASCII(quoted)
		for _, c := range escaped {
			require.Less(t, c, byte(0x7f), string(escaped))
		}
		var want, back string
		require.NoError(t, json.Unmarshal(quoted, &want))
		require.NoError(t, json.Unmarshal(escaped, &back))
		require.Equal(t, want, back)
	})
}
