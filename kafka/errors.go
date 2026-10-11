package kafka

import (
	"errors"
	"fmt"
)

// The errors of the Kafka consumer. Deserialization, format mismatch and
// output serializer failures reach the handler wrapped in a *RecordError
// that says which record and which field failed.
var (
	// ErrDeserialization is what a key or value that cannot be decoded
	// matches: bad base64, invalid UTF-8, malformed JSON, Avro or Protobuf,
	// a broken wire-format header. Python: KafkaConsumerDeserializationError.
	ErrDeserialization = errors.New("kafka: deserialization failed")

	// ErrInvalidWireFormat is what a schema registry header that cannot be
	// read matches. It matches ErrDeserialization too.
	ErrInvalidWireFormat = fmt.Errorf("%w: invalid wire format", ErrDeserialization)

	// ErrFormatMismatch is what a key or value matches when the schema
	// registry reports a data format other than the one the deserializer
	// reads. Python: KafkaConsumerDeserializationFormatMismatch.
	ErrFormatMismatch = errors.New("kafka: data format mismatch")

	// ErrOutputSerializer is what a failure of the function given to Map, or
	// of the Validate method of the decoded value, matches. Python declares
	// KafkaConsumerOutputSerializerError but never raises it.
	ErrOutputSerializer = errors.New("kafka: output serializer failed")

	// ErrMissingSchema is what a deserializer built without the schema it
	// needs returns. Python: KafkaConsumerMissingSchemaError.
	ErrMissingSchema = errors.New("kafka: missing schema")

	// ErrSchemaParse is what a deserializer built with a schema that does
	// not parse returns. Python: KafkaConsumerAvroSchemaParserError.
	ErrSchemaParse = errors.New("kafka: invalid schema")
)

// Field names the part of a record that failed.
type Field string

// The fields of a record that are deserialized.
const (
	FieldKey   Field = "key"
	FieldValue Field = "value"
)

// RecordError reports a key or value that could not be deserialized,
// validated or mapped. Err matches ErrDeserialization, ErrFormatMismatch or
// ErrOutputSerializer.
type RecordError struct {
	Field     Field
	Topic     string
	Partition int64
	Offset    int64
	Err       error
}

func (e *RecordError) Error() string {
	return fmt.Sprintf("kafka: %s of record %s-%d at offset %d: %v", e.Field, e.Topic, e.Partition, e.Offset, e.Err)
}

// Unwrap returns the cause.
func (e *RecordError) Unwrap() error { return e.Err }

// FormatMismatchError reports a key or value whose schema metadata names a
// data format other than the one the deserializer reads.
type FormatMismatchError struct {
	// Expected is the format of the deserializer.
	Expected Format
	// Actual is the format the schema registry reports.
	Actual Format
}

func (e *FormatMismatchError) Error() string {
	return fmt.Sprintf("kafka: expected %s data, the schema registry reports %s", e.Expected, e.Actual)
}

// Is makes every FormatMismatchError match ErrFormatMismatch.
func (*FormatMismatchError) Is(target error) bool { return target == ErrFormatMismatch }
