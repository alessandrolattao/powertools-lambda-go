package kafka

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// The wire formats of schema registry serializers.
//
// Confluent: magic byte 0x00, the schema ID as a big-endian uint32, then for
// Protobuf the message indexes, then the payload.
//
// AWS Glue: header version byte 0x03, a compression byte (0x00 none, 0x05
// zlib), the 16 bytes of the schema version UUID, then for Protobuf the
// message index as a varint, then the payload.
//
// When the event source mapping integrates with the registry, Lambda strips
// the header and reports the schema in the record's schema metadata; the
// Protobuf message indexes stay in the payload, and the protobuf
// deserializer removes them.
const (
	confluentMagicByte  = 0x00
	confluentHeaderSize = 5

	glueHeaderVersion = 0x03
	glueHeaderSize    = 18
)

// The compression codes of the Glue wire format.
const (
	GlueCompressionNone byte = 0x00
	GlueCompressionZlib byte = 0x05
)

// ParseConfluentHeader reads the 5-byte header of Confluent's wire format:
// it returns the schema ID and what follows the header.
func ParseConfluentHeader(data []byte) (schemaID uint32, rest []byte, err error) {
	if len(data) < confluentHeaderSize {
		return 0, nil, fmt.Errorf("%w: Confluent payloads start with a 5-byte header", ErrInvalidWireFormat)
	}
	if data[0] != confluentMagicByte {
		return 0, nil, fmt.Errorf("%w: expected Confluent magic byte 0x00, found 0x%02x", ErrInvalidWireFormat, data[0])
	}
	return binary.BigEndian.Uint32(data[1:confluentHeaderSize]), data[confluentHeaderSize:], nil
}

// ParseConfluentMessageIndexes reads the message indexes Confluent's
// Protobuf serializer writes before the message: they locate the message
// type in the schema's .proto file. The count and every index are zigzag
// varints, and the single byte 0x00 stands for [0], the first message type.
//
// Powertools Python reads them as plain varints, which goes wrong as soon as
// a schema has more than one message type; this function follows the
// Confluent specification.
func ParseConfluentMessageIndexes(data []byte) (indexes []int64, rest []byte, err error) {
	count, n := binary.Varint(data)
	if n <= 0 {
		return nil, nil, fmt.Errorf("%w: truncated message index count", ErrInvalidWireFormat)
	}
	data = data[n:]
	switch {
	case count == 0:
		return []int64{0}, data, nil
	case count < 0 || count > int64(len(data)):
		// Each index takes at least one byte: a larger count is corrupt, and
		// must not size an allocation.
		return nil, nil, fmt.Errorf("%w: invalid message index count %d", ErrInvalidWireFormat, count)
	}
	indexes = make([]int64, 0, count)
	for range count {
		index, n := binary.Varint(data)
		if n <= 0 {
			return nil, nil, fmt.Errorf("%w: truncated message index", ErrInvalidWireFormat)
		}
		indexes = append(indexes, index)
		data = data[n:]
	}
	return indexes, data, nil
}

// MaxGlueInflatedSize bounds what a zlib-compressed Glue payload may inflate
// to: Lambda delivers at most 6 MB of event, so a record value far beyond it
// is a decompression bomb, not a message, and would otherwise take the
// function down on every retry.
const MaxGlueInflatedSize = 64 << 20

// GlueHeader is the header of the AWS Glue Schema Registry wire format.
type GlueHeader struct {
	// SchemaVersionID identifies the schema version in the registry.
	SchemaVersionID uuid.UUID
	// Compression is GlueCompressionNone or GlueCompressionZlib.
	Compression byte
}

// ParseGlueHeader reads the 18-byte header of the AWS Glue Schema Registry
// wire format and returns it with the payload that follows, decompressed
// when the producer compressed it (up to MaxGlueInflatedSize).
func ParseGlueHeader(data []byte) (GlueHeader, []byte, error) {
	var header GlueHeader
	if len(data) < glueHeaderSize {
		return header, nil, fmt.Errorf("%w: Glue payloads start with an 18-byte header", ErrInvalidWireFormat)
	}
	if data[0] != glueHeaderVersion {
		return header, nil, fmt.Errorf("%w: expected Glue header version 3, found %d", ErrInvalidWireFormat, data[0])
	}
	header.Compression = data[1]
	copy(header.SchemaVersionID[:], data[2:glueHeaderSize])
	payload := data[glueHeaderSize:]
	switch header.Compression {
	case GlueCompressionNone:
		return header, payload, nil
	case GlueCompressionZlib:
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return header, nil, fmt.Errorf("%w: zlib: %w", ErrInvalidWireFormat, err)
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, MaxGlueInflatedSize+1))
		if err != nil {
			return header, nil, fmt.Errorf("%w: zlib: %w", ErrInvalidWireFormat, err)
		}
		if len(inflated) > MaxGlueInflatedSize {
			return header, nil, fmt.Errorf("%w: zlib payload inflates beyond %d bytes", ErrInvalidWireFormat, MaxGlueInflatedSize)
		}
		return header, inflated, nil
	default:
		return header, nil, fmt.Errorf("%w: unknown Glue compression %d", ErrInvalidWireFormat, header.Compression)
	}
}

// ConfluentWireFormat reads payloads that a Confluent schema registry
// serializer wrote (KafkaAvroSerializer, KafkaProtobufSerializer,
// KafkaJsonSchemaSerializer) with d, for event source mappings without the
// schema registry integration: it checks the magic byte and strips the
// 5-byte header, then hands d the rest with the schema metadata the
// integration would have reported. Powertools Python's
// schema_wire_format="CONFLUENT", which covers Avro only.
//
// A record that carries schema metadata went through the integration,
// which already stripped the header: its payload goes to d unchanged.
func ConfluentWireFormat[T any](d Deserializer[T]) Deserializer[T] {
	return deserializerFunc[T]{format: d.Format(), fn: func(data []byte, meta *SchemaMetadata) (T, error) {
		if meta != nil {
			return d.Deserialize(data, meta)
		}
		id, rest, err := ParseConfluentHeader(data)
		if err != nil {
			var zero T
			return zero, err
		}
		debug.Logger().Debug("deserializing a Confluent payload", "schemaId", id)
		return d.Deserialize(rest, &SchemaMetadata{DataFormat: d.Format(), SchemaID: strconv.FormatUint(uint64(id), 10)})
	}}
}

// GlueWireFormat reads payloads that an AWS Glue Schema Registry serializer
// wrote with d, for event source mappings without the schema registry
// integration: it checks and strips the 18-byte header, inflates a
// compressed payload, then hands d the rest with the schema metadata the
// integration would have reported.
//
// A record that carries schema metadata went through the integration,
// which already stripped the header: its payload goes to d unchanged.
func GlueWireFormat[T any](d Deserializer[T]) Deserializer[T] {
	return deserializerFunc[T]{format: d.Format(), fn: func(data []byte, meta *SchemaMetadata) (T, error) {
		if meta != nil {
			return d.Deserialize(data, meta)
		}
		header, payload, err := ParseGlueHeader(data)
		if err != nil {
			var zero T
			return zero, err
		}
		debug.Logger().Debug("deserializing a Glue payload", "schemaVersionId", header.SchemaVersionID.String())
		return d.Deserialize(payload, &SchemaMetadata{DataFormat: d.Format(), SchemaID: header.SchemaVersionID.String()})
	}}
}
