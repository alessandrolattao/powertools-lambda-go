// Package kafka processes the events of Kafka event source mappings (Amazon
// MSK, MSK Serverless and self-managed Apache Kafka), decoding the key and
// value of every record from base64 and deserializing them from JSON, Avro,
// Protocol Buffers or a Kafka primitive type: the Go side of Powertools
// Python's Kafka consumer utility.
//
// # Getting started
//
// Consumer adapts a handler of deserialized records into a Lambda handler,
// as Python's @kafka_consumer decorator does. The deserializers of the key
// and the value play the part of SchemaConfig:
//
//	func handle(ctx context.Context, records *kafka.ConsumerRecords[string, Order]) (string, error) {
//		for rec, err := range records.All() {
//			if err != nil {
//				return "", err
//			}
//			process(rec.Key, rec.Value)
//		}
//		return "ok", nil
//	}
//
//	func main() {
//		lambda.Start(kafka.Consumer(handle, kafka.String(), kafka.JSON[Order]()))
//	}
//
// NewConsumerRecords does the same without the adapter, and Deserialize
// works on one record, such as inside a batch processing record handler.
//
// # Deserializers
//
//   - String: UTF-8 text, Python's default when a field has no schema type.
//   - Bytes: the bytes as they are.
//   - JSON[T]: a JSON document into a T (Python's "JSON", followed by an
//     output serializer when T is a struct).
//   - Int16, Int32, Int64, Float32, Float64, Bool, UUID: what the Java
//     client's primitive serializers write. Python reads every such key as
//     a string, although its documentation lists integers.
//   - avro.New[T] (package kafka/avro): Avro, Python's "AVRO".
//   - protobuf.New[M] and protobuf.Dynamic (package kafka/protobuf):
//     Protocol Buffers, Python's "PROTOBUF".
//
// Map applies a function to what a deserializer returns, Python's output
// serializers; a decoded value whose type has a Validate() error method is
// validated, the way Pydantic models are. A custom format is any type that
// implements Deserializer.
//
// # Schema registries
//
// With the schema registry integration of the event source mapping (AWS
// Glue, Confluent Cloud or self-managed Confluent), Lambda validates every
// record, strips the registry's header and reports the schema in
// ConsumerRecord.KeySchemaMetadata and ValueSchemaMetadata. A deserializer
// whose Format differs from the reported data format fails with a
// *FormatMismatchError. Protobuf payloads keep the message indexes the
// registry serializers write; the protobuf package removes them.
//
// Without the integration, payloads reach the function with the header.
// ConfluentWireFormat and GlueWireFormat strip it before deserializing:
// Python's schema_wire_format="CONFLUENT", extended to every format and to
// AWS Glue. ParseConfluentHeader, ParseConfluentMessageIndexes and
// ParseGlueHeader read the headers for custom code.
//
// # Errors
//
// Records are deserialized as the iteration reaches them, and a record that
// fails comes with a *RecordError naming the field, topic, partition and
// offset; the iteration goes on with the next record. The cause matches one
// of:
//
//   - ErrDeserialization: the payload cannot be decoded (ErrInvalidWireFormat
//     for a broken registry header);
//   - ErrFormatMismatch: the schema registry reports another data format;
//   - ErrOutputSerializer: the function given to Map, or Validate, failed.
//
// ErrMissingSchema and ErrSchemaParse come from the constructors of
// deserializers that need a schema, when the function starts.
//
// # Events
//
// Event decodes the Lambda event itself. aws-lambda-go's events.KafkaEvent
// is not enough: it keeps records in a map, losing their order, it has no
// schema metadata, and it cannot tell a null value from an empty one.
// FromLambdaEvent converts one, for code that already has it.
//
// # Environment variables
//
// The package reads no variable of its own. POWERTOOLS_TRACE_MIDDLEWARES
// traces the Consumer adapter, as it does every middleware, and
// POWERTOOLS_DEBUG logs every deserialization.
//
// # Differences from Powertools Python
//
//   - Deserializers are typed: the key and value come out as K and V, not as
//     dictionaries, and the schema is parsed when the function starts
//     (ErrSchemaParse) rather than at the first record. There is no
//     deserializer cache: deserializers are built once.
//   - Confluent Protobuf message indexes are zigzag varints, as the Confluent
//     specification defines them; Python reads them as plain varints and
//     goes wrong as soon as a schema has more than one message type.
//   - A null value is a tombstone (ConsumerRecord.Tombstone) rather than a
//     deserialization error.
//   - Output serializers run whenever they are configured; Python ignores
//     them for a field without a schema type. Their errors match
//     ErrOutputSerializer; Python lets them escape unwrapped.
//   - Repeated header keys are all kept (Headers.Values) and keep their
//     case; Get finds the last one, ignoring case, as Python does.
//   - The format check compares each record's own metadata; Python's cache
//     compares later records with the format of the first one.
//   - Python's stateful ConsumerRecords.record becomes the All iterator
//     (iter.Pull2 gives a cursor).
package kafka
