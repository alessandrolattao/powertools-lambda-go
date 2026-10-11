package datamasking

import (
	"encoding/json/v2"
	"testing"
)

// FuzzErase feeds arbitrary JSON and expressions to field selection and
// erasing: whatever arrives in an event, Erase returns a JSON value or an
// error, never panics.
func FuzzErase(f *testing.F) {
	f.Add(`{"a":{"1":{"None":"hello"},"b":{"3":{"4":"goodbye"}}}}`, "a..'4'", uint8(0))
	f.Add(`{"address":[{"postcode":12345,"street":"a"},{"postcode":1}]}`, "$.address[?(@.postcode > 12000)]", uint8(1))
	f.Add(`{"address":[{"street":"a"}]}`, "address[*].street", uint8(2))
	f.Add(`{"address":[{"street":"a"}]}`, "address[-1:].street", uint8(3))
	f.Add(`[1,{"a":[null,true,1.5e300]}]`, "[*].a[0,1,2]", uint8(1))
	f.Add(`{"k":"v"}`, "$", uint8(2))
	masker := New(WithRaiseOnMissingField(false))
	f.Fuzz(func(t *testing.T, data, path string, mode uint8) {
		var opts []EraseOption
		switch mode % 4 {
		case 0:
			opts = []EraseOption{WithFields(path)}
		case 1:
			opts = []EraseOption{WithFields(path), WithDynamicMask()}
		case 2:
			opts = []EraseOption{WithMaskingRules(Rule{Path: path, CustomMask: "XX"})}
		default:
			opts = []EraseOption{WithFields(path), WithRegex(`(\w)\w*`, `\1***`)}
		}
		got, err := masker.Erase([]byte(data), opts...)
		if err != nil {
			return
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("result does not marshal: %v", err)
		}
	})
}

// FuzzCompilePath checks that no expression makes the parser panic.
func FuzzCompilePath(f *testing.F) {
	for _, path := range []string{
		"card_number", "address.postcode", "category..name", "address[*].street", "address[-1].street",
		"$.address[?(@.postcode > 12000)]", "a.'1'.None", "a..'4'", "'3'..True", "a[1:2:3]",
		"a[?(@.b == 'x' & !@.c)]", `a[?(@.b =~ "^x")]`, "a,b", "[0,1]",
	} {
		f.Add(path)
	}
	f.Fuzz(func(_ *testing.T, path string) {
		_, _ = compilePath(path) //nolint:errcheck // the property is the absence of panics
	})
}

// FuzzRegexMask checks Python replacement templates against arbitrary
// input.
func FuzzRegexMask(f *testing.F) {
	f.Add(`(.)(.*)(@.*)`, `\1****\3`, "johndoe@example.com")
	f.Add(`(?P<u>\w+)`, `\g<u>\g<0>\012\0`, "a b")
	f.Add(`x*`, `-`, "abxd")
	f.Fuzz(func(_ *testing.T, pattern, format, input string) {
		m, err := newRegexMask(pattern, format)
		if err != nil {
			return
		}
		_ = m.replace(input)
	})
}
