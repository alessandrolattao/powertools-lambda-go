package protobuf

import (
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/protobuf/internal/testpb"
)

// FuzzDeserialize feeds arbitrary payloads, under every framing, to the
// Protobuf deserializers: they return a message or an error, never panic.
func FuzzDeserialize(f *testing.F) {
	f.Add([]byte{0x0a, 0x03, 'a', 'n', 'n', 0x10, 0x05}, "")
	f.Add([]byte{0x04, 0x02, 0x00, 0x0a, 0x01, 'x'}, "456")
	f.Add([]byte{0x01, 0x0a, 0x04, 'u', '8', '5', '9'}, "7d55d475-2244-4485-8341-f74468c1e058")
	generated := New[*testpb.UserProfile]()
	dynamic := Dynamic(testpb.File_user_profile_proto.Messages().ByName("UserProfile"))
	asMap := AsMap(generated)
	f.Fuzz(func(_ *testing.T, data []byte, schemaID string) {
		meta := &kafka.SchemaMetadata{DataFormat: kafka.FormatProtobuf, SchemaID: schemaID}
		_, _ = generated.Deserialize(data, meta) //nolint:errcheck // the property is the absence of panics
		_, _ = dynamic.Deserialize(data, meta)   //nolint:errcheck // the property is the absence of panics
		_, _ = asMap.Deserialize(data, meta)     //nolint:errcheck // the property is the absence of panics
	})
}
