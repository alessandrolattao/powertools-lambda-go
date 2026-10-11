// Package avro deserializes Kafka keys and values written in Apache Avro, the
// format Powertools recommends for Kafka: Powertools Python's
// value_schema_type="AVRO" (and key_schema_type).
//
// New parses the writer schema once, when the function starts, rather than
// at the first record as Python does; an invalid schema is a startup error
// that matches kafka.ErrSchemaParse.
//
//	var orders = kafka.Must(avro.New[Order](orderSchema))
//
//	lambda.Start(kafka.Consumer(handle, kafka.String(), orders))
//
// The value is decoded into T: a struct whose fields carry `avro:"name"`
// tags, or map[string]any (or any) for the dictionaries Python returns.
// Logical types decode to their Go equivalents (time.Time for dates and
// timestamps, *big.Rat for decimals). Data after the first datum is ignored,
// as Python's DatumReader ignores it.
//
// Payloads that still carry a schema registry header, because the event
// source mapping does not integrate with the registry, are read by wrapping
// the deserializer: kafka.ConfluentWireFormat(d) for Confluent (Python's
// schema_wire_format="CONFLUENT") or kafka.GlueWireFormat(d) for AWS Glue.
//
// Avro decoding is github.com/twmb/avro.
package avro

import (
	"fmt"
	"strings"

	twmbavro "github.com/twmb/avro"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/kafka"
)

// New returns a deserializer of Avro binary data written with schema (the
// .avsc JSON document), decoded into a T.
func New[T any](schema string) (kafka.Deserializer[T], error) {
	if strings.TrimSpace(schema) == "" {
		return nil, fmt.Errorf("%w: an Avro deserializer needs the writer schema", kafka.ErrMissingSchema)
	}
	parsed, err := twmbavro.Parse(schema)
	if err != nil {
		return nil, fmt.Errorf("%w: avro: %w", kafka.ErrSchemaParse, err)
	}
	return deserializer[T]{schema: parsed}, nil
}

type deserializer[T any] struct {
	schema *twmbavro.Schema
}

func (deserializer[T]) Format() kafka.Format { return kafka.FormatAvro }

func (d deserializer[T]) Deserialize(data []byte, _ *kafka.SchemaMetadata) (T, error) {
	debug.Logger().Debug("deserializing data with AVRO format")
	var v T
	if _, err := d.schema.Decode(data, &v); err != nil {
		var zero T
		return zero, fmt.Errorf("%w: avro: %w", kafka.ErrDeserialization, err)
	}
	return v, nil
}
