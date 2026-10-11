package protobuf_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/protobuf"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/protobuf/internal/testpb"
)

func loadEvent(t *testing.T, name string) *kafka.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", name))
	require.NoError(t, err)
	var event kafka.Event
	require.NoError(t, json.Unmarshal(data, &event))
	return &event
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(m)
	require.NoError(t, err)
	return data
}

// TestPlainSampleEvent reads the Protobuf sample event of the Powertools
// Python documentation.
func TestPlainSampleEvent(t *testing.T) {
	t.Parallel()
	records := kafka.NewConsumerRecords(loadEvent(t, "protobuf.json"), kafka.String(), protobuf.New[*testpb.User]())
	count := 0
	for rec, err := range records.All() {
		require.NoError(t, err)
		assert.Equal(t, "123", rec.Key)
		assert.Equal(t, "powertools", rec.Value.GetName())
		assert.Equal(t, int32(5), rec.Value.GetAge())
		count++
	}
	assert.Equal(t, 1, count)
}

// TestConfluentSampleEvent reads the same message framed four ways: plain,
// with the [0] message index shortcut, with the [1, 0] index list, and with
// a Glue schema version ID.
func TestConfluentSampleEvent(t *testing.T) {
	t.Parallel()
	want := &testpb.UserProfile{
		UserId:      "123",
		Name:        "Test",
		Email:       "test@gmx.com",
		Age:         10,
		SignupDate:  "2025-06-20",
		Tags:        []string{"tag1", "tag2"},
		Score:       12,
		Preferences: map[string]string{"theme": "light"},
		Address:     &testpb.Address{Street: "Mythenquai", City: "Zurich", Zip: "8002"},
	}
	records := kafka.NewConsumerRecords(loadEvent(t, "protobuf_confluent.json"), kafka.String(), protobuf.New[*testpb.UserProfile]())
	count := 0
	for rec, err := range records.All() {
		require.NoError(t, err)
		// Read without zigzag decoding, as Python does, the [1, 0] record
		// loses its first fields.
		assert.True(t, proto.Equal(want, rec.Value), "offset %d: %v", rec.Offset, rec.Value)
		count++
	}
	assert.Equal(t, 4, count)
}

// TestGlueSampleEvent reads an event recorded from an event source mapping
// integrated with AWS Glue Schema Registry.
func TestGlueSampleEvent(t *testing.T) {
	t.Parallel()
	records := kafka.NewConsumerRecords(loadEvent(t, "protobuf_glue.json"), kafka.String(), protobuf.New[*testpb.UserProfile]())
	var ages []int32
	for rec, err := range records.All() {
		require.NoError(t, err)
		assert.Equal(t, "alice@example.com", rec.Value.GetEmail())
		assert.Equal(t, "Alice", rec.Value.GetName())
		ages = append(ages, rec.Value.GetAge())
	}
	assert.Equal(t, []int32{54, 40, 74, 19}, ages)
}

func TestDynamic(t *testing.T) {
	t.Parallel()
	desc := testpb.File_user_proto.Messages().ByName("User")
	users := protobuf.Dynamic(desc)
	assert.Equal(t, kafka.FormatProtobuf, users.Format())

	got, err := users.Deserialize(marshal(t, &testpb.User{Name: "ann", Age: 3}), nil)
	require.NoError(t, err)
	assert.Equal(t, "ann", got.Get(desc.Fields().ByName("name")).String())
	assert.Equal(t, int64(3), got.Get(desc.Fields().ByName("age")).Int())
}

func TestNewRejectsDynamicMessages(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { protobuf.New[*dynamicpb.Message]() })
}

func TestAsMap(t *testing.T) {
	t.Parallel()
	profiles := protobuf.AsMap(protobuf.New[*testpb.UserProfile]())
	assert.Equal(t, kafka.FormatProtobuf, profiles.Format())

	got, err := profiles.Deserialize(marshal(t, &testpb.UserProfile{
		UserId: "u1", Age: 30, Visits: 1 << 40, Tags: []string{"a"},
		Address: &testpb.Address{City: "Rome"},
	}), nil)
	require.NoError(t, err)
	// MessageToDict: proto field names, 64-bit integers as strings, fields
	// at their default value left out.
	assert.Equal(t, map[string]any{
		"userId":  "u1",
		"age":     30.0,
		"visits":  "1099511627776",
		"tags":    []any{"a"},
		"address": map[string]any{"city": "Rome"},
	}, got)

	keys := protobuf.AsMap(protobuf.New[*testpb.Key]())
	key, err := keys.Deserialize(marshal(t, &testpb.Key{UserId: "u1"}), nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"user_id": "u1"}, key, "the .proto name, not the JSON name")
}

func TestInvalidData(t *testing.T) {
	t.Parallel()
	users := protobuf.New[*testpb.User]()
	_, err := users.Deserialize([]byte("invalid protobuf data"), nil)
	require.ErrorIs(t, err, kafka.ErrDeserialization)

	_, err = users.Deserialize([]byte{0x02, 0x80}, &kafka.SchemaMetadata{SchemaID: "1"})
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)

	_, err = users.Deserialize([]byte{0x80}, &kafka.SchemaMetadata{SchemaID: "7d55d475-2244-4485-8341-f74468c1e058"})
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)

	// Protobuf has no schema check: another message type reads without
	// error, with what its fields make of the data, as in Python.
	key, err := protobuf.New[*testpb.Key]().Deserialize(marshal(t, &testpb.User{Name: "ann", Age: 3}), nil)
	require.NoError(t, err)
	assert.Empty(t, key.GetUserId())
}

func TestFormatMismatch(t *testing.T) {
	t.Parallel()
	record := &kafka.Record{
		Topic:               "users",
		Value:               new(base64.StdEncoding.EncodeToString(marshal(t, &testpb.User{Name: "ann"}))),
		ValueSchemaMetadata: &kafka.SchemaMetadata{DataFormat: kafka.FormatAvro, SchemaID: "1234"},
	}
	_, err := kafka.Deserialize(record, kafka.String(), protobuf.New[*testpb.User]())
	require.ErrorIs(t, err, kafka.ErrFormatMismatch)
}

func TestStripMessageIndexes(t *testing.T) {
	t.Parallel()
	glueID := "7d55d475-2244-4485-8341-f74468c1e058"
	cases := []struct {
		name string
		data []byte
		meta *kafka.SchemaMetadata
		want []byte
	}{
		{name: "no metadata", data: []byte{0x0a, 0x01}, want: []byte{0x0a, 0x01}},
		{name: "no schema ID", data: []byte{0x0a}, meta: &kafka.SchemaMetadata{DataFormat: kafka.FormatProtobuf}, want: []byte{0x0a}},
		{name: "Confluent first type", data: []byte{0x00, 0x0a}, meta: &kafka.SchemaMetadata{SchemaID: "1"}, want: []byte{0x0a}},
		{name: "Confluent nested type", data: []byte{0x04, 0x02, 0x00, 0x0a}, meta: &kafka.SchemaMetadata{SchemaID: "1"}, want: []byte{0x0a}},
		{name: "Glue one byte index", data: []byte{0x01, 0x0a}, meta: &kafka.SchemaMetadata{SchemaID: glueID}, want: []byte{0x0a}},
		{name: "Glue two byte index", data: []byte{0x96, 0x01, 0x0a}, meta: &kafka.SchemaMetadata{SchemaID: glueID}, want: []byte{0x0a}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := protobuf.StripMessageIndexes(tc.data, tc.meta)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWireFormatsWithoutIntegration reads payloads that still carry the
// registry header, as they arrive without the schema registry integration.
func TestWireFormatsWithoutIntegration(t *testing.T) {
	t.Parallel()
	message := marshal(t, &testpb.UserProfile{Email: "ann@example.com", Age: 33})

	confluent := binary.BigEndian.AppendUint32([]byte{0}, 12)
	confluent = append(confluent, 0x02, 0x02) // message indexes [1]
	confluent = append(confluent, message...)
	got, err := kafka.ConfluentWireFormat(protobuf.New[*testpb.UserProfile]()).Deserialize(confluent, nil)
	require.NoError(t, err)
	assert.Equal(t, "ann@example.com", got.GetEmail())

	id := uuid.MustParse("7d55d475-2244-4485-8341-f74468c1e058")
	glue := append([]byte{3, 0}, id[:]...)
	glue = append(glue, 0x01) // message index 1
	glue = append(glue, message...)
	got, err = kafka.GlueWireFormat(protobuf.New[*testpb.UserProfile]()).Deserialize(glue, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(33), got.GetAge())
}
