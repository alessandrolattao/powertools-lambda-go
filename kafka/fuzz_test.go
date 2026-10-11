package kafka

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
)

// FuzzEvent feeds arbitrary events to the decoder and the record iteration:
// whatever Lambda, or anyone else, sends, they return records or errors,
// never panic.
func FuzzEvent(f *testing.F) {
	for _, name := range []string{"msk.json", "self_managed.json", "json.json", "protobuf_glue.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"records":{"t-0":[{"key":null,"value":null,"headers":[{"k":[-1,255]}]}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var event Event
		if err := json.Unmarshal(data, &event); err != nil {
			return
		}
		records := NewConsumerRecords(&event, String(), JSON[any]())
		n := 0
		for rec, err := range records.All() {
			n++
			if rec == nil {
				t.Fatalf("nil record, err %v", err)
			}
		}
		if n != event.Len() {
			t.Fatalf("iterated %d records, Len says %d", n, event.Len())
		}
		if _, err := json.Marshal(&event); err != nil {
			t.Fatalf("a decoded event does not encode: %v", err)
		}
	})
}

// FuzzDeserializers feeds arbitrary bytes to every built-in deserializer.
func FuzzDeserializers(f *testing.F) {
	f.Add([]byte(`{"name":"a"}`), "1234")
	f.Add([]byte{0, 0, 0, 0, 7, '{', '}'}, "")
	f.Add([]byte{3, 5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 'x'}, "7d55d475-2244-4485-8341-f74468c1e058")
	f.Add([]byte{0xff, 0xfe}, "")
	f.Fuzz(func(_ *testing.T, data []byte, schemaID string) {
		var meta *SchemaMetadata
		if schemaID != "" {
			meta = &SchemaMetadata{SchemaID: schemaID}
		}
		deserialize(String(), data, meta)
		deserialize(Bytes(), data, meta)
		deserialize(JSON[any](), data, meta)
		deserialize(JSON[map[string]int](), data, meta)
		deserialize(Int16(), data, meta)
		deserialize(Int32(), data, meta)
		deserialize(Int64(), data, meta)
		deserialize(Float32(), data, meta)
		deserialize(Float64(), data, meta)
		deserialize(Bool(), data, meta)
		deserialize(UUID(), data, meta)
		deserialize(ConfluentWireFormat(JSON[any]()), data, nil)
		deserialize(GlueWireFormat(Bytes()), data, nil)
	})
}

func deserialize[T any](d Deserializer[T], data []byte, meta *SchemaMetadata) {
	_, _ = d.Deserialize(data, meta) //nolint:errcheck // the property is the absence of panics
}

// FuzzWireFormats feeds arbitrary bytes to the schema registry header
// parsers.
func FuzzWireFormats(f *testing.F) {
	f.Add([]byte{0x04, 0x02, 0x00, 0x0a})
	f.Add([]byte{0, 0, 0, 0, 1, 0, 0x0a})
	f.Add([]byte{3, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, rest, err := ParseConfluentHeader(data); err == nil && len(rest) != len(data)-confluentHeaderSize {
			t.Fatal("the Confluent header is 5 bytes")
		}
		if indexes, rest, err := ParseConfluentMessageIndexes(data); err == nil && (len(indexes) == 0 || len(rest) >= len(data)) {
			t.Fatalf("indexes %v consumed nothing", indexes)
		}
		_, _, _ = ParseGlueHeader(data) //nolint:errcheck // the property is the absence of panics
	})
}
