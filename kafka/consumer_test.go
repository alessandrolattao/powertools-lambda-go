package kafka_test

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
)

type user struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func b64(s string) *string { return new(base64.StdEncoding.EncodeToString([]byte(s))) }

func record(key, value *string) *kafka.Record {
	return &kafka.Record{Topic: "orders", Partition: 3, Offset: 42, Timestamp: 1750547462087, Key: key, Value: value}
}

func TestConsumerThroughLambda(t *testing.T) {
	t.Parallel()
	payload, err := os.ReadFile(filepath.Join("testdata", "json.json"))
	require.NoError(t, err)

	handle := func(_ context.Context, records *kafka.ConsumerRecords[string, user]) ([]string, error) {
		var seen []string
		for rec, err := range records.All() {
			if err != nil {
				return nil, err
			}
			seen = append(seen, rec.Key+":"+rec.Value.Name)
			assert.Equal(t, 5, rec.Value.Age)
			assert.Equal(t, "python-with-avro-doc", rec.Topic)
			assert.Equal(t, int64(5), rec.Partition)
			assert.Equal(t, int64(1750547462087), rec.Timestamp.UnixMilli())
		}
		assert.Equal(t, 1, records.Len())
		assert.Equal(t, kafka.EventSourceMSK, records.Event.EventSource)
		return seen, nil
	}

	// lambda.NewHandler decodes the event as lambda.Start does, with
	// encoding/json.
	h := lambda.NewHandler(kafka.Consumer(handle, kafka.String(), kafka.JSON[user]()))
	response, err := h.Invoke(t.Context(), payload)
	require.NoError(t, err)
	assert.JSONEq(t, `["123:Powertools"]`, string(response))
}

func TestConsumerReturnsTheHandlerError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	h := kafka.Consumer(func(context.Context, *kafka.ConsumerRecords[string, string]) (int, error) {
		return 0, boom
	}, kafka.String(), kafka.String())
	_, err := h(t.Context(), loadEvent(t, "self_managed.json"))
	require.ErrorIs(t, err, boom)
}

func TestConsumerPanicsWithoutDeserializers(t *testing.T) {
	t.Parallel()
	handle := func(context.Context, *kafka.ConsumerRecords[string, string]) (int, error) { return 0, nil }
	assert.Panics(t, func() { kafka.Consumer(handle, nil, kafka.String()) })
	assert.Panics(t, func() { kafka.NewConsumerRecords[string, string](&kafka.Event{}, kafka.String(), nil) })
}

func TestDeserializeKeyAndValueSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		key, value    *string
		wantKey       string
		wantHasKey    bool
		wantValue     string
		wantTombstone bool
	}{
		{name: "both", key: b64("k"), value: b64("v"), wantKey: "k", wantHasKey: true, wantValue: "v"},
		{name: "no key", value: b64("v"), wantValue: "v"},
		{name: "empty key", key: new(""), value: b64("v"), wantValue: "v"},
		{name: "empty value", key: b64("k"), value: new(""), wantKey: "k", wantHasKey: true},
		{name: "tombstone", key: b64("k"), wantKey: "k", wantHasKey: true, wantTombstone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, err := kafka.Deserialize(record(tc.key, tc.value), kafka.String(), kafka.String())
			require.NoError(t, err)
			assert.Equal(t, tc.wantKey, rec.Key)
			assert.Equal(t, tc.wantHasKey, rec.HasKey)
			assert.Equal(t, tc.wantValue, rec.Value)
			assert.Equal(t, tc.wantTombstone, rec.Tombstone)
		})
	}
}

func TestDeserializeExposesMetadata(t *testing.T) {
	t.Parallel()
	event := loadEvent(t, "msk.json")
	records := kafka.NewConsumerRecords(event, kafka.String(), kafka.Bytes())
	for rec, err := range records.All() {
		require.NoError(t, err)
		assert.Equal(t, int64(15), rec.Offset)
		assert.Equal(t, kafka.TimestampCreateTime, rec.TimestampType)
		assert.JSONEq(t, `{"key":"value"}`, string(rec.Value))
		assert.Equal(t, &kafka.SchemaMetadata{DataFormat: kafka.FormatAvro, SchemaID: "1234"}, rec.ValueSchemaMetadata)
		assert.Equal(t, &kafka.SchemaMetadata{DataFormat: kafka.FormatAvro, SchemaID: "1234"}, rec.KeySchemaMetadata)
		assert.Equal(t, "eyJrZXkiOiJ2YWx1ZSJ9", *rec.Raw.Value)
		assert.NotEmpty(t, rec.Headers)
	}
}

func TestFormatMismatch(t *testing.T) {
	t.Parallel()
	event := loadEvent(t, "msk.json")
	records := kafka.NewConsumerRecords(event, kafka.String(), kafka.JSON[map[string]any]())

	count := 0
	for rec, err := range records.All() {
		count++
		require.ErrorIs(t, err, kafka.ErrFormatMismatch)
		require.NotErrorIs(t, err, kafka.ErrDeserialization)
		mismatch, ok := errors.AsType[*kafka.FormatMismatchError](err)
		require.True(t, ok)
		assert.Equal(t, kafka.FormatJSON, mismatch.Expected)
		assert.Equal(t, kafka.FormatAvro, mismatch.Actual)

		recordErr, ok := errors.AsType[*kafka.RecordError](err)
		require.True(t, ok)
		assert.Equal(t, kafka.FieldValue, recordErr.Field)
		assert.Equal(t, rec.Raw.Topic, recordErr.Topic)
		assert.Equal(t, int64(15), recordErr.Offset)
		assert.Contains(t, err.Error(), "value of record "+rec.Raw.TopicPartition()+" at offset 15")

		assert.Equal(t, rec.Raw.Topic, rec.Topic, "a failed record still carries its metadata")
		assert.Nil(t, rec.Value)
	}
	assert.Equal(t, 4, count, "the iteration goes on after a failed record")
}

type failing struct{}

func (failing) Format() kafka.Format { return "" }

func (failing) Deserialize([]byte, *kafka.SchemaMetadata) (string, error) {
	return "", errors.New("custom failure")
}

func TestDeserializationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value *string
		d     kafka.Deserializer[string]
		want  error
	}{
		{name: "invalid base64", value: new("!!!"), d: kafka.String(), want: kafka.ErrDeserialization},
		{name: "invalid UTF-8", value: new(base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe})), d: kafka.String(), want: kafka.ErrDeserialization},
		{name: "invalid JSON", value: b64("{"), d: kafka.JSON[string](), want: kafka.ErrDeserialization},
		{name: "JSON of another type", value: b64(`{"a":1}`), d: kafka.JSON[string](), want: kafka.ErrDeserialization},
		{name: "custom deserializer", value: b64("x"), d: failing{}, want: kafka.ErrDeserialization},
		{name: "output serializer", value: b64("x"), d: kafka.Map(kafka.String(), func(string) (string, error) {
			return "", errors.New("rejected")
		}), want: kafka.ErrOutputSerializer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, err := kafka.Deserialize(record(nil, tc.value), kafka.String(), tc.d)
			require.ErrorIs(t, err, tc.want)
			require.NotNil(t, rec)
			recordErr, ok := errors.AsType[*kafka.RecordError](err)
			require.True(t, ok)
			assert.Equal(t, kafka.FieldValue, recordErr.Field)
		})
	}
}

func TestDeserializeKeyAndValueFailing(t *testing.T) {
	t.Parallel()
	_, err := kafka.Deserialize(record(new("!!!"), new("!!!")), kafka.String(), kafka.String())
	require.ErrorIs(t, err, kafka.ErrDeserialization)
	joined, ok := err.(interface{ Unwrap() []error })
	require.True(t, ok)
	fields := make([]kafka.Field, 0, 2)
	for _, e := range joined.Unwrap() {
		recordErr, isRecordErr := errors.AsType[*kafka.RecordError](e)
		require.True(t, isRecordErr)
		fields = append(fields, recordErr.Field)
	}
	assert.Equal(t, []kafka.Field{kafka.FieldKey, kafka.FieldValue}, fields)

	rec, err := kafka.Deserialize(record(new("!!!"), b64("v")), kafka.String(), kafka.String())
	require.Error(t, err)
	recordErr, ok := errors.AsType[*kafka.RecordError](err)
	require.True(t, ok)
	assert.Equal(t, kafka.FieldKey, recordErr.Field)
	assert.False(t, rec.HasKey)
	assert.Equal(t, "v", rec.Value)
}

func TestAllStopsWhenTheLoopBreaks(t *testing.T) {
	t.Parallel()
	records := kafka.NewConsumerRecords(loadEvent(t, "msk.json"), kafka.String(), kafka.String())
	count := 0
	for range records.All() {
		count++
		break
	}
	assert.Equal(t, 1, count)
}

func TestMap(t *testing.T) {
	t.Parallel()
	type order struct {
		OrderID string
		Total   float64
	}
	// The cross-language pattern of the Python docs: normalize a Java
	// producer's field names into the function's own type.
	orders := kafka.Map(kafka.JSON[map[string]any](), func(m map[string]any) (order, error) {
		id, ok := m["orderId"].(string)
		if !ok {
			return order{}, errors.New("orderId is not a string")
		}
		total, ok := m["totalAmount"].(float64)
		if !ok {
			return order{}, errors.New("totalAmount is not a number")
		}
		return order{OrderID: id, Total: total}, nil
	})
	assert.Equal(t, kafka.FormatJSON, orders.Format(), "Map keeps the format of what it wraps")

	rec, err := kafka.Deserialize(record(nil, b64(`{"orderId":"o-1","totalAmount":12.5}`)), kafka.String(), orders)
	require.NoError(t, err)
	assert.Equal(t, order{OrderID: "o-1", Total: 12.5}, rec.Value)

	_, err = kafka.Deserialize(record(nil, b64(`{`)), kafka.String(), orders)
	require.ErrorIs(t, err, kafka.ErrDeserialization, "a failure of the wrapped deserializer is not the mapping's")
	require.NotErrorIs(t, err, kafka.ErrOutputSerializer)
}

type checked struct {
	Name string `json:"name"`
}

func (c checked) Validate() error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	return nil
}

type checkedPtr struct {
	Age int `json:"age"`
}

func (c *checkedPtr) Validate() error {
	if c.Age < 0 {
		return errors.New("age must not be negative")
	}
	return nil
}

func TestValidate(t *testing.T) {
	t.Parallel()
	_, err := kafka.Deserialize(record(nil, b64(`{"name":"ann"}`)), kafka.String(), kafka.JSON[checked]())
	require.NoError(t, err)

	_, err = kafka.Deserialize(record(nil, b64(`{}`)), kafka.String(), kafka.JSON[checked]())
	require.ErrorIs(t, err, kafka.ErrOutputSerializer)
	assert.Contains(t, err.Error(), "name is required")

	_, err = kafka.Deserialize(record(nil, b64(`{"age":-1}`)), kafka.String(), kafka.JSON[checkedPtr]())
	require.ErrorIs(t, err, kafka.ErrOutputSerializer, "a Validate method on the pointer counts")

	_, err = kafka.Deserialize(record(nil, b64(`{"age":-1}`)), kafka.String(), kafka.JSON[*checkedPtr]())
	require.ErrorIs(t, err, kafka.ErrOutputSerializer)

	rec, err := kafka.Deserialize(record(nil, b64(`null`)), kafka.String(), kafka.JSON[*checkedPtr]())
	require.NoError(t, err, "a nil pointer is not validated")
	assert.Nil(t, rec.Value)
}

func TestPrimitives(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("7d55d475-2244-4485-8341-f74468c1e058")
	cases := []struct {
		name    string
		decode  func([]byte) (any, error)
		data    []byte
		want    any
		wantErr bool
	}{
		{name: "int16", decode: deserializeWith(kafka.Int16()), data: []byte{0xff, 0xfe}, want: int16(-2)},
		{name: "int32", decode: deserializeWith(kafka.Int32()), data: []byte{0, 0, 1, 0}, want: int32(256)},
		{name: "int64", decode: deserializeWith(kafka.Int64()), data: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, want: int64(-1)},
		{name: "float32", decode: deserializeWith(kafka.Float32()), data: []byte{0x3f, 0xc0, 0, 0}, want: float32(1.5)},
		{name: "float64", decode: deserializeWith(kafka.Float64()), data: []byte{0x40, 0x09, 0x21, 0xfb, 0x54, 0x44, 0x2d, 0x18}, want: math.Pi},
		{name: "bool true", decode: deserializeWith(kafka.Bool()), data: []byte{1}, want: true},
		{name: "bool false", decode: deserializeWith(kafka.Bool()), data: []byte{0}, want: false},
		{name: "uuid", decode: deserializeWith(kafka.UUID()), data: []byte(id.String()), want: id},
		{name: "bytes", decode: deserializeWith(kafka.Bytes()), data: []byte{0, 1}, want: []byte{0, 1}},
		{name: "string", decode: deserializeWith(kafka.String()), data: []byte("ë"), want: "ë"},
		{name: "int32 too short", decode: deserializeWith(kafka.Int32()), data: []byte{1}, wantErr: true},
		{name: "int64 too long", decode: deserializeWith(kafka.Int64()), data: make([]byte, 9), wantErr: true},
		{name: "bool not 0 or 1", decode: deserializeWith(kafka.Bool()), data: []byte{2}, wantErr: true},
		{name: "bool empty", decode: deserializeWith(kafka.Bool()), data: nil, wantErr: true},
		{name: "uuid malformed", decode: deserializeWith(kafka.UUID()), data: []byte("nope"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.decode(tc.data)
			if tc.wantErr {
				require.ErrorIs(t, err, kafka.ErrDeserialization)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func deserializeWith[T any](d kafka.Deserializer[T]) func([]byte) (any, error) {
	return func(data []byte) (any, error) {
		return d.Deserialize(data, nil)
	}
}

func TestPrimitiveKeyThroughRecords(t *testing.T) {
	t.Parallel()
	key := base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0, 0, 0, 0x30, 0x39})
	rec, err := kafka.Deserialize(record(&key, b64(`{"name":"a","age":1}`)), kafka.Int64(), kafka.JSON[user]())
	require.NoError(t, err)
	assert.Equal(t, int64(12345), rec.Key)
	assert.Equal(t, user{Name: "a", Age: 1}, rec.Value)
}

func TestJSONOptions(t *testing.T) {
	t.Parallel()
	strict := kafka.JSON[user](json.RejectUnknownMembers(true))
	_, err := strict.Deserialize([]byte(`{"name":"a","extra":1}`), nil)
	require.ErrorIs(t, err, kafka.ErrDeserialization)

	anything, err := kafka.JSON[any]().Deserialize([]byte(`{"a":[1,"x",null,true]}`), nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": []any{1.0, "x", nil, true}}, anything)
}

func TestMust(t *testing.T) {
	t.Parallel()
	assert.NotNil(t, kafka.Must(kafka.String(), nil))
	assert.Panics(t, func() { kafka.Must[string](nil, kafka.ErrSchemaParse) })
}
