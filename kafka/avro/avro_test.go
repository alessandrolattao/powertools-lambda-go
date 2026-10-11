package avro_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	twmbavro "github.com/twmb/avro"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/avro"
)

// userSchema is the schema of the Powertools Python Kafka examples.
const userSchema = `{
	"type": "record",
	"name": "User",
	"namespace": "com.example",
	"fields": [
		{"name": "name", "type": "string"},
		{"name": "age", "type": "int"}
	]
}`

type User struct {
	Name string `avro:"name"`
	Age  int    `avro:"age"`
}

func encode(t *testing.T, schema string, v any) []byte {
	t.Helper()
	data, err := twmbavro.MustParse(schema).Encode(v)
	require.NoError(t, err)
	return data
}

func TestNewNeedsASchema(t *testing.T) {
	t.Parallel()
	_, err := avro.New[User]("  ")
	require.ErrorIs(t, err, kafka.ErrMissingSchema)

	_, err = avro.New[User](`{"type": "record", "name": "User", "fields": [{"name": "age", "type": "nope"}]}`)
	require.ErrorIs(t, err, kafka.ErrSchemaParse)

	_, err = avro.New[User](`{not json`)
	require.ErrorIs(t, err, kafka.ErrSchemaParse)
}

func TestDeserialize(t *testing.T) {
	t.Parallel()
	data := encode(t, userSchema, &User{Name: "Powertools", Age: 5})

	users := kafka.Must(avro.New[User](userSchema))
	assert.Equal(t, kafka.FormatAvro, users.Format())
	got, err := users.Deserialize(data, nil)
	require.NoError(t, err)
	assert.Equal(t, User{Name: "Powertools", Age: 5}, got)

	// The dictionary Powertools Python returns.
	generic := kafka.Must(avro.New[map[string]any](userSchema))
	m, err := generic.Deserialize(data, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "Powertools", "age": int32(5)}, m)

	// Trailing data after the datum is ignored, as Python's DatumReader does.
	got, err = users.Deserialize(append(data, 0xff, 0xff), nil)
	require.NoError(t, err)
	assert.Equal(t, "Powertools", got.Name)

	_, err = users.Deserialize([]byte{0x14, 'x'}, nil)
	require.ErrorIs(t, err, kafka.ErrDeserialization)
}

func TestLogicalTypes(t *testing.T) {
	t.Parallel()
	const schema = `{
		"type": "record", "name": "OrderEvent", "namespace": "com.example.orders",
		"fields": [
			{"name": "orderId", "type": "string"},
			{"name": "orderDate", "type": {"type": "long", "logicalType": "timestamp-millis"}},
			{"name": "coupon", "type": ["null", "string"], "default": null}
		]
	}`
	type orderEvent struct {
		OrderID   string    `avro:"orderId"`
		OrderDate time.Time `avro:"orderDate"`
		Coupon    *string   `avro:"coupon"`
	}
	when := time.UnixMilli(1750547462087).UTC()
	data := encode(t, schema, &orderEvent{OrderID: "o-1", OrderDate: when})

	got, err := kafka.Must(avro.New[orderEvent](schema)).Deserialize(data, nil)
	require.NoError(t, err)
	assert.Equal(t, "o-1", got.OrderID)
	assert.True(t, when.Equal(got.OrderDate))
	assert.Nil(t, got.Coupon)
}

// TestGlueFramedSampleEvent reads the Avro sample event of the Powertools
// Python documentation: recorded without the schema registry integration,
// its value still carries the AWS Glue header.
func TestGlueFramedSampleEvent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "avro_glue_framed.json"))
	require.NoError(t, err)
	var event kafka.Event
	require.NoError(t, json.Unmarshal(data, &event))

	users := kafka.GlueWireFormat(kafka.Must(avro.New[User](userSchema)))
	records := kafka.NewConsumerRecords(&event, kafka.String(), users)
	count := 0
	for rec, err := range records.All() {
		require.NoError(t, err)
		assert.Equal(t, "123", rec.Key)
		assert.Equal(t, User{Name: "Powertools", Age: 5}, rec.Value)
		count++
	}
	assert.Equal(t, 1, count)
}

func TestConfluentWireFormat(t *testing.T) {
	t.Parallel()
	const keySchema = `{"type": "record", "name": "Key", "fields": [{"name": "id", "type": "long"}]}`
	type key struct {
		ID int64 `avro:"id"`
	}
	frame := func(schemaID uint32, body []byte) *string {
		data := binary.BigEndian.AppendUint32([]byte{0}, schemaID)
		return new(base64.StdEncoding.EncodeToString(append(data, body...)))
	}
	record := &kafka.Record{
		Topic: "users",
		Key:   frame(1, encode(t, keySchema, &key{ID: 42})),
		Value: frame(2, encode(t, userSchema, &User{Name: "ann", Age: 7})),
	}

	keys := kafka.ConfluentWireFormat(kafka.Must(avro.New[key](keySchema)))
	users := kafka.ConfluentWireFormat(kafka.Must(avro.New[User](userSchema)))
	rec, err := kafka.Deserialize(record, keys, users)
	require.NoError(t, err)
	assert.Equal(t, key{ID: 42}, rec.Key)
	assert.Equal(t, User{Name: "ann", Age: 7}, rec.Value)

	record.Value = new(base64.StdEncoding.EncodeToString(encode(t, userSchema, &User{Name: "ann"})))
	_, err = kafka.Deserialize(record, keys, users)
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat, "a payload without the header")
}

func TestFormatMismatch(t *testing.T) {
	t.Parallel()
	record := &kafka.Record{
		Topic:               "users",
		Value:               new(base64.StdEncoding.EncodeToString(encode(t, userSchema, &User{Name: "ann"}))),
		ValueSchemaMetadata: &kafka.SchemaMetadata{DataFormat: kafka.FormatJSON, SchemaID: "1"},
	}
	_, err := kafka.Deserialize(record, kafka.String(), kafka.Must(avro.New[User](userSchema)))
	require.ErrorIs(t, err, kafka.ErrFormatMismatch)

	record.ValueSchemaMetadata.DataFormat = kafka.FormatAvro
	rec, err := kafka.Deserialize(record, kafka.String(), kafka.Must(avro.New[User](userSchema)))
	require.NoError(t, err)
	assert.Equal(t, "ann", rec.Value.Name)
}
