package avro

import (
	"testing"

	twmbavro "github.com/twmb/avro"
)

// fuzzSchema exercises every Avro type the decoder handles.
const fuzzSchema = `{
	"type": "record", "name": "Everything",
	"fields": [
		{"name": "s", "type": "string"},
		{"name": "i", "type": "int"},
		{"name": "l", "type": "long"},
		{"name": "f", "type": "float"},
		{"name": "d", "type": "double"},
		{"name": "b", "type": "boolean"},
		{"name": "raw", "type": "bytes"},
		{"name": "u", "type": ["null", "string", "long"]},
		{"name": "e", "type": {"type": "enum", "name": "Color", "symbols": ["RED", "GREEN"]}},
		{"name": "x", "type": {"type": "fixed", "name": "Four", "size": 4}},
		{"name": "a", "type": {"type": "array", "items": "string"}},
		{"name": "m", "type": {"type": "map", "values": "int"}},
		{"name": "t", "type": {"type": "long", "logicalType": "timestamp-millis"}},
		{"name": "dec", "type": {"type": "bytes", "logicalType": "decimal", "precision": 10, "scale": 2}}
	]
}`

// FuzzDeserialize feeds arbitrary bytes to the Avro deserializer: it returns
// a value or an error, never panics.
func FuzzDeserialize(f *testing.F) {
	seed, err := twmbavro.MustParse(fuzzSchema).Encode(map[string]any{
		"s": "x", "i": 1, "l": 2, "f": 1.5, "d": 2.5, "b": true, "raw": []byte{1},
		"u": "y", "e": "GREEN", "x": []byte{1, 2, 3, 4}, "a": []any{"p"}, "m": map[string]any{"k": 3},
		"t": 1750547462087, "dec": []byte{0x04, 0xd2},
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{0x02, 'x'})
	generic, err := New[any](fuzzSchema)
	if err != nil {
		f.Fatal(err)
	}
	typed, err := New[map[string]any](fuzzSchema)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = generic.Deserialize(data, nil) //nolint:errcheck // the property is the absence of panics
		_, _ = typed.Deserialize(data, nil)   //nolint:errcheck // the property is the absence of panics
	})
}
