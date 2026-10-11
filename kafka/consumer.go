package kafka

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// ConsumerRecord is a Kafka record with its key and value deserialized.
type ConsumerRecord[K, V any] struct {
	Topic     string
	Partition int64
	Offset    int64
	Timestamp time.Time
	// TimestampType is TimestampCreateTime or TimestampLogAppendTime.
	TimestampType string

	// Key is the deserialized key; HasKey is false, and Key the zero K, when
	// the record has no key (absent, null or empty).
	Key    K
	HasKey bool
	// Value is the deserialized value; Tombstone is true, and Value the zero
	// V, when the value is null.
	Value     V
	Tombstone bool

	// Headers holds the headers, decoded to bytes.
	Headers Headers
	// KeySchemaMetadata and ValueSchemaMetadata describe the schemas the
	// schema registry integration validated the key and value against; nil
	// without the integration.
	KeySchemaMetadata   *SchemaMetadata
	ValueSchemaMetadata *SchemaMetadata

	// Raw is the record as the event carries it, with the base64 key and
	// value: Powertools Python's original_key, original_value and
	// original_headers.
	Raw *Record
}

// Deserialize decodes the key of record with key and its value with value.
// When either fails, it returns the record with what it could decode and a
// *RecordError per failed field, so that the record can still be reported or
// sent to a dead-letter queue.
//
// A decoded key or value whose type has a Validate() error method, on the
// value or its pointer, is validated: a failure matches ErrOutputSerializer.
func Deserialize[K, V any](record *Record, key Deserializer[K], value Deserializer[V]) (*ConsumerRecord[K, V], error) {
	out := &ConsumerRecord[K, V]{
		Topic:               record.Topic,
		Partition:           record.Partition,
		Offset:              record.Offset,
		Timestamp:           record.Time(),
		TimestampType:       record.TimestampType,
		Headers:             record.Headers,
		KeySchemaMetadata:   record.KeySchemaMetadata,
		ValueSchemaMetadata: record.ValueSchemaMetadata,
		Raw:                 record,
	}
	var keyErr, valueErr error
	if record.Key != nil && *record.Key != "" {
		debug.Logger().Debug("deserializing key field", "topic", record.Topic, "offset", record.Offset)
		out.Key, keyErr = decodeField(key, *record.Key, record.KeySchemaMetadata)
		out.HasKey = keyErr == nil
	}
	if record.Value == nil {
		out.Tombstone = true
	} else {
		debug.Logger().Debug("deserializing value field", "topic", record.Topic, "offset", record.Offset)
		out.Value, valueErr = decodeField(value, *record.Value, record.ValueSchemaMetadata)
	}
	switch {
	case keyErr != nil && valueErr != nil:
		return out, errors.Join(record.fieldError(FieldKey, keyErr), record.fieldError(FieldValue, valueErr))
	case keyErr != nil:
		return out, record.fieldError(FieldKey, keyErr)
	case valueErr != nil:
		return out, record.fieldError(FieldValue, valueErr)
	default:
		return out, nil
	}
}

func (r *Record) fieldError(field Field, err error) error {
	return &RecordError{Field: field, Topic: r.Topic, Partition: r.Partition, Offset: r.Offset, Err: err}
}

// decodeField checks the data format, decodes the base64, deserializes and
// validates one field.
func decodeField[T any](d Deserializer[T], encoded string, meta *SchemaMetadata) (T, error) {
	var zero T
	if meta != nil && meta.DataFormat != "" && d.Format() != "" && meta.DataFormat != d.Format() {
		return zero, &FormatMismatchError{Expected: d.Format(), Actual: meta.DataFormat}
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return zero, fmt.Errorf("%w: base64: %w", ErrDeserialization, err)
	}
	v, err := d.Deserialize(data, meta)
	if err != nil {
		// A custom deserializer may return any error: it still is a
		// deserialization failure to whoever handles records.
		if !errors.Is(err, ErrDeserialization) && !errors.Is(err, ErrFormatMismatch) && !errors.Is(err, ErrOutputSerializer) {
			err = fmt.Errorf("%w: %w", ErrDeserialization, err)
		}
		return zero, err
	}
	if err := validate(v); err != nil {
		return zero, fmt.Errorf("%w: %w", ErrOutputSerializer, err)
	}
	return v, nil
}

// validator is what a type implements to have its decoded values checked,
// the hook every Powertools utility that decodes into user types calls.
type validator interface {
	Validate() error
}

func validate[T any](v T) error {
	if val, ok := any(v).(validator); ok {
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil
		}
		return val.Validate()
	}
	if val, ok := any(&v).(validator); ok {
		return val.Validate()
	}
	return nil
}

// ConsumerRecords is an event whose records are deserialized as they are
// iterated: what a Consumer handler receives, Powertools Python's
// ConsumerRecords.
type ConsumerRecords[K, V any] struct {
	// Event is the event as Lambda delivered it.
	Event *Event

	key   Deserializer[K]
	value Deserializer[V]
}

// NewConsumerRecords wraps event, deserializing keys with key and values with
// value. It panics if either deserializer is nil.
func NewConsumerRecords[K, V any](event *Event, key Deserializer[K], value Deserializer[V]) *ConsumerRecords[K, V] {
	mustDeserializers(key, value)
	return &ConsumerRecords[K, V]{Event: event, key: key, value: value}
}

func mustDeserializers[K, V any](key Deserializer[K], value Deserializer[V]) {
	if key == nil || value == nil {
		panic("kafka: nil key or value deserializer")
	}
}

// All returns the records in the order of the event, each deserialized when
// the iteration reaches it. A record that fails comes with a *RecordError;
// the iteration goes on with the next one unless the loop breaks.
func (r *ConsumerRecords[K, V]) All() iter.Seq2[*ConsumerRecord[K, V], error] {
	return func(yield func(*ConsumerRecord[K, V], error) bool) {
		for record := range r.Event.All() {
			if !yield(Deserialize(record, r.key, r.value)) {
				return
			}
		}
	}
}

// Len returns the number of records.
func (r *ConsumerRecords[K, V]) Len() int {
	return r.Event.Len()
}

// Consumer adapts handler, which processes deserialized records, into a
// Lambda handler of Kafka events: Powertools Python's @kafka_consumer, the
// deserializers playing the part of SchemaConfig. Keys are deserialized with
// key and values with value; kafka.String() for both is the bare decorator.
//
// Records are deserialized as handler iterates them, so a failure surfaces
// on the record that has it. Like every Powertools middleware, the adapter
// shows in traces as "## kafka_consumer" when POWERTOOLS_TRACE_MIDDLEWARES is
// on. It panics if either deserializer is nil.
func Consumer[K, V, R any](handler middleware.Handler[*ConsumerRecords[K, V], R], key Deserializer[K], value Deserializer[V]) middleware.Handler[*Event, R] {
	mustDeserializers(key, value)
	adapted := func(ctx context.Context, event *Event) (R, error) {
		return handler(ctx, &ConsumerRecords[K, V]{Event: event, key: key, value: value})
	}
	trace := middleware.New("kafka_consumer", func(ctx context.Context, event *Event, next middleware.Handler[*Event, R]) (R, error) {
		return next(ctx, event)
	})
	return trace(adapted)
}
