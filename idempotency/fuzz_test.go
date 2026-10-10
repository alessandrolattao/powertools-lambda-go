package idempotency_test

import (
	"encoding/json/jsontext"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
)

// FuzzCanonicalJSON feeds arbitrary payloads to the canonical encoder: it
// never panics, its output is JSON whenever the numbers are finite, and
// canonicalizing it again changes nothing.
func FuzzCanonicalJSON(f *testing.F) {
	f.Add([]byte(`{"b":1,"a":[1.5,"x",null,true]}`))
	f.Add([]byte(`"é😀\u0007"`))
	f.Add([]byte(`[1e400, -0, 0.0001, 1e16]`))
	f.Add([]byte(`123456789012345678901234567890`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		canonical, err := idempotency.CanonicalJSON(raw)
		if err != nil {
			return
		}
		text := string(canonical)
		if strings.Contains(text, "Infinity") || strings.Contains(text, "NaN") {
			return // Python's extension of JSON, as json.dumps writes it
		}
		if !jsontext.Value(canonical).IsValid() {
			t.Fatalf("canonical form of %q is not JSON: %s", raw, canonical)
		}
		again, err := idempotency.CanonicalJSON(canonical)
		if err != nil {
			t.Fatalf("canonical form of %q does not parse: %v", raw, err)
		}
		if string(again) != text {
			t.Fatalf("canonicalizing twice changed %s into %s", canonical, again)
		}
	})
}

// FuzzPythonFloat checks that every float renders as digits that read back
// as the same float, with a fraction or an exponent so that Python reads it
// as a float.
func FuzzPythonFloat(f *testing.F) {
	for _, v := range []float64{0, 1, 0.1, 1e16, 1e-5, 123456.789, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v float64) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		raw, err := idempotency.CanonicalJSON([]byte(strconv.FormatFloat(v, 'e', -1, 64)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.ContainsAny(text, ".e") {
			t.Fatalf("%v renders as %s, which Python reads as an integer", v, text)
		}
		back, err := strconv.ParseFloat(text, 64)
		if err != nil || back != v {
			t.Fatalf("%v renders as %s, which reads back as %v (%v)", v, text, back, err)
		}
	})
}
