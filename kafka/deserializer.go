package kafka

import (
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"math"
	"unicode/utf8"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Deserializer turns the bytes of a key or value into a T. The records layer
// decodes the base64 of the event first and checks the data format the
// schema registry reports against Format, so a deserializer only sees bytes.
//
// The built-in deserializers are String, Bytes, JSON and the Kafka primitive
// types here, Avro in the avro subpackage and Protocol Buffers in the
// protobuf subpackage. Map turns any of them into a deserializer of another
// type, and ConfluentWireFormat and GlueWireFormat strip the header a schema
// registry serializer writes.
type Deserializer[T any] interface {
	// Format is the data format the deserializer reads, or "" when it reads
	// any: the schema registry must not report another one.
	Format() Format
	// Deserialize decodes data. meta is the schema metadata of the field,
	// nil when the event source mapping does not integrate with a schema
	// registry.
	Deserialize(data []byte, meta *SchemaMetadata) (T, error)
}

// deserializerFunc is a Deserializer made of a format and a function.
type deserializerFunc[T any] struct {
	format Format
	fn     func(data []byte, meta *SchemaMetadata) (T, error)
}

func (d deserializerFunc[T]) Format() Format { return d.format }

func (d deserializerFunc[T]) Deserialize(data []byte, meta *SchemaMetadata) (T, error) {
	return d.fn(data, meta)
}

// String reads UTF-8 text: Powertools Python's default deserializer, used
// when a field has no schema type. Invalid UTF-8 is an error, as it is in
// Python.
func String() Deserializer[string] {
	return deserializerFunc[string]{fn: func(data []byte, _ *SchemaMetadata) (string, error) {
		debug.Logger().Debug("deserializing data with primitive types")
		if !utf8.Valid(data) {
			return "", fmt.Errorf("%w: invalid UTF-8", ErrDeserialization)
		}
		return string(data), nil
	}}
}

// Bytes returns the bytes as they are.
func Bytes() Deserializer[[]byte] {
	return deserializerFunc[[]byte]{fn: func(data []byte, _ *SchemaMetadata) ([]byte, error) {
		return data, nil
	}}
}

// JSON reads a JSON document into a T with encoding/json/v2, and the options
// given. With T = any, the result is what Powertools Python's JSON
// deserializer returns: maps, slices, strings, float64s, bools and nil.
func JSON[T any](opts ...json.Options) Deserializer[T] {
	return deserializerFunc[T]{format: FormatJSON, fn: func(data []byte, _ *SchemaMetadata) (T, error) {
		debug.Logger().Debug("deserializing data with JSON format")
		var v T
		if err := json.Unmarshal(data, &v, opts...); err != nil {
			return v, fmt.Errorf("%w: json: %w", ErrDeserialization, err)
		}
		return v, nil
	}}
}

// The Kafka primitive types, as the Java client's serializers write them.
// Powertools Python reads every key without a schema as a string; a key
// written by LongSerializer, for one, needs Int64.

// Int16 reads a big-endian 16-bit integer, Kafka's ShortSerializer.
func Int16() Deserializer[int16] {
	return fixed(2, "int16", func(b []byte) int16 { return int16(binary.BigEndian.Uint16(b)) }) //nolint:gosec // G115: two's complement reinterpretation, as Java writes it
}

// Int32 reads a big-endian 32-bit integer, Kafka's IntegerSerializer.
func Int32() Deserializer[int32] {
	return fixed(4, "int32", func(b []byte) int32 { return int32(binary.BigEndian.Uint32(b)) }) //nolint:gosec // G115: two's complement reinterpretation, as Java writes it
}

// Int64 reads a big-endian 64-bit integer, Kafka's LongSerializer.
func Int64() Deserializer[int64] {
	return fixed(8, "int64", func(b []byte) int64 { return int64(binary.BigEndian.Uint64(b)) }) //nolint:gosec // G115: two's complement reinterpretation, as Java writes it
}

// Float32 reads a big-endian IEEE 754 single, Kafka's FloatSerializer.
func Float32() Deserializer[float32] {
	return fixed(4, "float32", func(b []byte) float32 { return math.Float32frombits(binary.BigEndian.Uint32(b)) })
}

// Float64 reads a big-endian IEEE 754 double, Kafka's DoubleSerializer.
func Float64() Deserializer[float64] {
	return fixed(8, "float64", func(b []byte) float64 { return math.Float64frombits(binary.BigEndian.Uint64(b)) })
}

// Bool reads one byte, 1 for true and 0 for false, Kafka's
// BooleanSerializer.
func Bool() Deserializer[bool] {
	return deserializerFunc[bool]{fn: func(data []byte, _ *SchemaMetadata) (bool, error) {
		if len(data) != 1 || data[0] > 1 {
			return false, fmt.Errorf("%w: a bool is one byte, 0 or 1", ErrDeserialization)
		}
		return data[0] == 1, nil
	}}
}

// UUID reads a UUID in its text form, Kafka's UUIDSerializer.
func UUID() Deserializer[uuid.UUID] {
	return deserializerFunc[uuid.UUID]{fn: func(data []byte, _ *SchemaMetadata) (uuid.UUID, error) {
		id, err := uuid.Parse(string(data))
		if err != nil {
			return id, fmt.Errorf("%w: uuid: %w", ErrDeserialization, err)
		}
		return id, nil
	}}
}

func fixed[T any](size int, name string, decode func([]byte) T) Deserializer[T] {
	return deserializerFunc[T]{fn: func(data []byte, _ *SchemaMetadata) (T, error) {
		if len(data) != size {
			var zero T
			return zero, fmt.Errorf("%w: an %s is %d bytes, got %d", ErrDeserialization, name, size, len(data))
		}
		return decode(data), nil
	}}
}

// Map turns a deserializer of A into one of B, applying fn to what d
// returns: Powertools Python's output serializer. An error from fn matches
// ErrOutputSerializer.
func Map[A, B any](d Deserializer[A], fn func(A) (B, error)) Deserializer[B] {
	return deserializerFunc[B]{format: d.Format(), fn: func(data []byte, meta *SchemaMetadata) (B, error) {
		a, err := d.Deserialize(data, meta)
		if err != nil {
			var zero B
			return zero, err
		}
		b, err := fn(a)
		if err != nil {
			return b, fmt.Errorf("%w: %w", ErrOutputSerializer, err)
		}
		return b, nil
	}}
}

// Must returns d, and panics if err is not nil. It is meant for package
// level deserializers built from schemas known to be valid:
//
//	var users = kafka.Must(avro.New[User](userSchema))
func Must[T any](d Deserializer[T], err error) Deserializer[T] {
	if err != nil {
		panic(err)
	}
	return d
}
