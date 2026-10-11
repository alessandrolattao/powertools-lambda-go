package kafka_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
)

// confluent frames payload the way Confluent's serializers do; indexes, when
// not nil, are written as the Protobuf serializer writes them.
func confluent(schemaID uint32, indexes []int64, payload []byte) []byte {
	out := binary.BigEndian.AppendUint32([]byte{0}, schemaID)
	out = append(out, messageIndexes(indexes)...)
	return append(out, payload...)
}

func messageIndexes(indexes []int64) []byte {
	switch {
	case indexes == nil:
		return nil
	case len(indexes) == 1 && indexes[0] == 0:
		return []byte{0}
	}
	out := binary.AppendVarint(nil, int64(len(indexes)))
	for _, index := range indexes {
		out = binary.AppendVarint(out, index)
	}
	return out
}

// glue frames payload the way AWS Glue's serializers do.
func glue(t *testing.T, id uuid.UUID, compress bool, payload []byte) []byte {
	t.Helper()
	compression := kafka.GlueCompressionNone
	if compress {
		compression = kafka.GlueCompressionZlib
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		_, err := w.Write(payload)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		payload = buf.Bytes()
	}
	out := append([]byte{3, compression}, id[:]...)
	return append(out, payload...)
}

func TestParseConfluentHeader(t *testing.T) {
	t.Parallel()
	id, rest, err := kafka.ParseConfluentHeader(confluent(70000, nil, []byte("body")))
	require.NoError(t, err)
	assert.Equal(t, uint32(70000), id)
	assert.Equal(t, []byte("body"), rest)

	_, _, err = kafka.ParseConfluentHeader([]byte{0, 0, 0})
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)
	require.ErrorIs(t, err, kafka.ErrDeserialization)
	assert.Contains(t, err.Error(), "5-byte header")

	_, _, err = kafka.ParseConfluentHeader([]byte{1, 0, 0, 0, 1, 'x'})
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)
	assert.Contains(t, err.Error(), "magic byte")
}

func TestParseConfluentMessageIndexes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		data    []byte
		want    []int64
		rest    []byte
		wantErr bool
	}{
		{name: "first message type shortcut", data: []byte{0x00, 0x0a}, want: []int64{0}, rest: []byte{0x0a}},
		// The upstream test payload: count 2, indexes [1, 0]. Read without
		// zigzag decoding, as Python does, 0x04 is a count of 4 and eats the
		// first two bytes of the message.
		{name: "nested message", data: []byte{0x04, 0x02, 0x00, 0x0a, 0x03}, want: []int64{1, 0}, rest: []byte{0x0a, 0x03}},
		{name: "second message type", data: append(messageIndexes([]int64{1}), 0x0a), want: []int64{1}, rest: []byte{0x0a}},
		{name: "large index", data: messageIndexes([]int64{300, 2}), want: []int64{300, 2}, rest: []byte{}},
		{name: "empty", data: nil, wantErr: true},
		{name: "negative count", data: []byte{0x01, 0x00}, wantErr: true},
		{name: "count beyond the data", data: []byte{0x08, 0x02}, wantErr: true},
		{name: "huge count", data: binary.AppendVarint(nil, 1<<40), wantErr: true},
		{name: "truncated index", data: []byte{0x02, 0x80}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			indexes, rest, err := kafka.ParseConfluentMessageIndexes(tc.data)
			if tc.wantErr {
				require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, indexes)
			assert.Equal(t, tc.rest, rest)
		})
	}
}

func TestParseGlueHeader(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("574f6a9a-9548-4dea-86a3-d82c0479a116")
	for _, compress := range []bool{false, true} {
		header, payload, err := kafka.ParseGlueHeader(glue(t, id, compress, []byte("payload")))
		require.NoError(t, err)
		assert.Equal(t, id, header.SchemaVersionID)
		assert.Equal(t, []byte("payload"), payload)
	}

	cases := map[string][]byte{
		"short":               {3, 0, 1},
		"wrong version":       append([]byte{2, 0}, id[:]...),
		"unknown compression": append([]byte{3, 9}, id[:]...),
		"corrupt zlib":        append(append([]byte{3, 5}, id[:]...), 'x', 'y'),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := kafka.ParseGlueHeader(data)
			require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)
		})
	}
}

func TestConfluentWireFormat(t *testing.T) {
	t.Parallel()
	users := kafka.ConfluentWireFormat(kafka.JSON[user]())
	assert.Equal(t, kafka.FormatJSON, users.Format())

	got, err := users.Deserialize(confluent(7, nil, []byte(`{"name":"ann","age":3}`)), nil)
	require.NoError(t, err)
	assert.Equal(t, user{Name: "ann", Age: 3}, got)

	_, err = users.Deserialize([]byte(`{"name":"ann"}`), nil)
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)

	// With schema metadata, the integration has already stripped the header.
	got, err = users.Deserialize([]byte(`{"name":"bob","age":4}`), &kafka.SchemaMetadata{DataFormat: kafka.FormatJSON, SchemaID: "7"})
	require.NoError(t, err)
	assert.Equal(t, user{Name: "bob", Age: 4}, got)
}

func TestConfluentWireFormatHandsOverTheSchemaID(t *testing.T) {
	t.Parallel()
	var seen *kafka.SchemaMetadata
	d := kafka.ConfluentWireFormat[[]byte](metaProbe{seen: &seen, d: kafka.Bytes()})
	got, err := d.Deserialize(confluent(70000, nil, []byte("x")), nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("x"), got)
	require.NotNil(t, seen)
	assert.Equal(t, "70000", seen.SchemaID)
	assert.Equal(t, kafka.RegistryConfluent, seen.Registry())
}

// metaProbe records the schema metadata it is handed.
type metaProbe struct {
	seen **kafka.SchemaMetadata
	d    kafka.Deserializer[[]byte]
}

func (p metaProbe) Format() kafka.Format { return p.d.Format() }

func (p metaProbe) Deserialize(data []byte, meta *kafka.SchemaMetadata) ([]byte, error) {
	*p.seen = meta
	return p.d.Deserialize(data, meta)
}

func TestGlueWireFormat(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("7d55d475-2244-4485-8341-f74468c1e058")
	var seen *kafka.SchemaMetadata
	users := kafka.GlueWireFormat[[]byte](metaProbe{seen: &seen, d: kafka.Bytes()})

	got, err := users.Deserialize(glue(t, id, true, []byte(`{"name":"ann"}`)), nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"ann"}`, string(got))
	require.NotNil(t, seen)
	assert.Equal(t, id.String(), seen.SchemaID)
	assert.Equal(t, kafka.RegistryGlue, seen.Registry())

	_, err = users.Deserialize([]byte(`{"name":"ann"}`), nil)
	require.ErrorIs(t, err, kafka.ErrInvalidWireFormat)

	got, err = users.Deserialize([]byte("as is"), &kafka.SchemaMetadata{SchemaID: id.String()})
	require.NoError(t, err)
	assert.Equal(t, []byte("as is"), got)
}
