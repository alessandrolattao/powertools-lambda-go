# Kafka Consumer

The Kafka consumer utility decodes and deserializes the records of Kafka
events, from Amazon MSK, MSK Serverless and self-managed Apache Kafka, into
your own types.

## Key features

- Deserialization of keys and values from JSON, Avro, Protocol Buffers and the
  Kafka primitive types, each field configured on its own
- Records in the order of the event, with metadata, decoded headers and the
  schema registry metadata
- Support for event source mappings with and without the schema registry
  integration (AWS Glue, Confluent Cloud, self-managed Confluent)
- Payloads with a Confluent or Glue wire-format header read without the
  integration
- Output serializers and validation of the decoded values
- Errors per record, so that one bad record does not fail the batch

## Terminology

**Event source mapping (ESM)**: the Lambda feature that reads from Kafka and
invokes the function with batches of records.

**Key and value**: the two parts of a Kafka record, base64-encoded in the
Lambda event, deserialized independently.

**Deserializer**: what turns the bytes of a key or value into a Go value, a
`kafka.Deserializer[T]`. The key and value deserializers given to the
consumer play the part of Python's `SchemaConfig`.

**Output serializer**: a function applied to what a deserializer returns, to
shape the data for the business logic (`kafka.Map`).

**Schema registry**: a service storing the schemas of the topics; the event
source mapping can validate records against it.

## Getting started

### Installation

```bash
go get github.com/alessandrolattao/powertools-lambda-go
```

JSON and the primitive types are in package `kafka`; Avro is in
`kafka/avro` (on `github.com/twmb/avro`), Protocol Buffers in
`kafka/protobuf` (on `google.golang.org/protobuf`). Go compiles only what is
imported.

### Required resources

A Lambda function with an Amazon MSK, MSK Serverless or self-managed Kafka
event source mapping. With the schema registry integration (provisioned mode
only), choose the `SOURCE` event record format to receive the payloads in
their format, or `JSON` to have Lambda convert them to JSON.

### Processing Kafka events

`kafka.Consumer` adapts a handler of deserialized records into a Lambda
handler: Python's `@kafka_consumer`.

```go
type Order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

func handle(_ context.Context, records *kafka.ConsumerRecords[string, Order]) (int, error) {
	processed := 0
	for rec, err := range records.All() {
		if errors.Is(err, kafka.ErrDeserialization) {
			// Send rec.Raw to a dead-letter queue and go on.
			continue
		}
		if err != nil {
			return processed, err
		}
		correlation, _ := rec.Headers.Get("correlation-id")
		fmt.Printf("customer %s ordered %s for %.2f (%s)\n", rec.Key, rec.Value.ID, rec.Value.Total, correlation)
		processed++
	}
	return processed, nil
}

func main() {
	lambda.Start(kafka.Consumer(handle, kafka.String(), kafka.JSON[Order]()))
}
```

With Avro, parse the schema once, when the function starts:

```go
const userSchema = `{
	"type": "record", "name": "User", "namespace": "com.example",
	"fields": [{"name": "name", "type": "string"}, {"name": "age", "type": "int"}]
}`

type User struct {
	Name string `avro:"name"`
	Age  int    `avro:"age"`
}

var users = kafka.Must(avro.New[User](userSchema))

func main() {
	lambda.Start(kafka.Consumer(handle, kafka.String(), users))
}
```

`avro.New[map[string]any]` returns the dictionaries Python returns.

With Protocol Buffers, use the generated type:

```go
var users = protobuf.New[*userpb.User]()
```

`protobuf.Dynamic(descriptor)` reads messages known only by their descriptor,
and `protobuf.AsMap(d)` returns Python's dictionary (`MessageToDict` with the
`.proto` field names).

### Deserializing key and value

Key and value are configured independently, each with its own deserializer:
a JSON key with an Avro value, a primitive key with a Protobuf value, and so
on. `kafka.String()` for both is the bare `@kafka_consumer`.

### Handling primitive types

| Deserializer | Producer serializer (Java client) |
| ------------ | --------------------------------- |
| `kafka.String()` | `StringSerializer`, Python's default |
| `kafka.Bytes()` | `ByteArraySerializer` |
| `kafka.Int16()`, `Int32()`, `Int64()` | `ShortSerializer`, `IntegerSerializer`, `LongSerializer` |
| `kafka.Float32()`, `Float64()` | `FloatSerializer`, `DoubleSerializer` |
| `kafka.Bool()` | `BooleanSerializer` |
| `kafka.UUID()` | `UUIDSerializer` |

Python reads every key without a schema as a string, so the integers its
documentation mentions fail to decode; in Go, `kafka.Int64()` reads the key
of a `LongSerializer` producer.

### Message format support and comparison

| Format | Deserializer | Schema |
| ------ | ------------ | ------ |
| JSON | `kafka.JSON[T]()` | none |
| Avro | `avro.New[T](schema)` | the writer schema (`.avsc`) |
| Protocol Buffers | `protobuf.New[M]()` | the generated type |

For new applications, prefer Avro or Protocol Buffers: schema evolution,
smaller messages, faster processing.

## Advanced

### Accessing record metadata

| Field | Description |
| ----- | ----------- |
| `Topic`, `Partition`, `Offset` | Where the record comes from |
| `Timestamp` | `time.Time`; `Timestamp.UnixMilli()` is Python's integer |
| `TimestampType` | `CREATE_TIME` or `LOG_APPEND_TIME` |
| `Headers` | Decoded headers: `Get` (case-insensitive, last value), `Values`, `All` |
| `Key`, `HasKey` | Deserialized key; `HasKey` is false without a key |
| `Value`, `Tombstone` | Deserialized value; `Tombstone` is true for a null value |
| `KeySchemaMetadata`, `ValueSchemaMetadata` | `DataFormat` and `SchemaID` from the schema registry integration |
| `Raw` | The record as the event carries it: base64 key and value (Python's `original_key`, `original_value`, `original_headers`) |

`ConsumerRecords.Event` is the event itself: `EventSource`, `EventSourceARN`,
`BootstrapServers` and `BootstrapServerList()`.

### Using a schema-registry wire-format prefix without the integration

When a schema registry serializer writes a payload, a header precedes it.
With the ESM schema registry integration, Lambda strips it and reports the
schema in the record's metadata. Without the integration, the header reaches
the function: wrap the deserializer to strip it.

```go
// Python: SchemaConfig(value_schema_type="AVRO", value_schema=..., value_schema_wire_format="CONFLUENT")
users := kafka.ConfluentWireFormat(kafka.Must(avro.New[User](userSchema)))

// AWS Glue Schema Registry serializers, compressed or not.
users = kafka.GlueWireFormat(kafka.Must(avro.New[User](userSchema)))
```

Both work with every format (Avro, Protobuf, JSON Schema), and pass the
payload through unchanged for records that carry schema metadata, whose
header the integration already stripped. `ParseConfluentHeader`,
`ParseConfluentMessageIndexes` and `ParseGlueHeader` read the headers for
custom code.

Protobuf payloads keep the message indexes the registry serializers write,
even through the integration; the protobuf deserializer removes them, reading
Confluent's as zigzag varints, as the Confluent specification defines them.

### Custom output serializers

`kafka.Map` applies a function to what a deserializer returns, for example to
normalize a Java producer's field names:

```go
type order struct {
	OrderID string
	Total   float64
}

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
```

Decoding straight into a struct replaces Python's dataclass and Pydantic
serializers. A decoded value whose type has a `Validate() error` method (on
the value or on its pointer) is validated, the way a Pydantic model is.

### Error handling

Records are deserialized as the iteration reaches them. A record that fails
comes with a `*kafka.RecordError` naming the field, topic, partition and
offset, and the iteration goes on with the next record. The cause matches:

| Error | Python | Cause |
| ----- | ------ | ----- |
| `kafka.ErrDeserialization` | `KafkaConsumerDeserializationError` | The payload cannot be decoded (bad base64, JSON, Avro, Protobuf, UTF-8) |
| `kafka.ErrInvalidWireFormat` | the same, "Invalid Confluent wire format" | A registry header cannot be read; matches `ErrDeserialization` too |
| `kafka.ErrFormatMismatch` (`*FormatMismatchError`) | `KafkaConsumerDeserializationFormatMismatch` | The schema registry reports another data format than the deserializer reads |
| `kafka.ErrOutputSerializer` | `KafkaConsumerOutputSerializerError` | The `Map` function or `Validate` failed |
| `kafka.ErrMissingSchema` | `KafkaConsumerMissingSchemaError` | A deserializer built without its schema |
| `kafka.ErrSchemaParse` | `KafkaConsumerAvroSchemaParserError` | A schema that does not parse |

The last two come from the constructors, when the function starts, rather
than at the first record.

```go
for rec, err := range records.All() {
	switch {
	case errors.Is(err, kafka.ErrDeserialization), errors.Is(err, kafka.ErrFormatMismatch):
		sendToDLQ(rec.Raw)
		continue
	case err != nil:
		return err
	}
	process(rec.Value)
}
```

### Integrating with Idempotency

Use the coordinates of a record, topic, partition and offset, as the
idempotency key, so that a retried batch does not process a record twice.
`rec.Raw.TopicPartition()` gives the `topic-partition` name.

### Handling partial batch failures

Without partial batch responses, Lambda retries the whole batch when the
function fails. To retry only the failed records, use the batch processing
utility on the Kafka event and deserialize inside the record handler with
`kafka.Deserialize(record, keyDeserializer, valueDeserializer)`.
`kafka.FromLambdaEvent` and `kafka.FromLambdaRecord` convert aws-lambda-go's
`events.KafkaEvent`, losing what it does not keep: record order, schema
metadata, null values.

### Best practices

- **Large messages**: store the data in Amazon S3 and send its reference;
  split payloads into smaller messages; give the function more memory.
- **Batch size**: larger batches (100 to 500 records) for simple processing,
  smaller ones (10 to 50) when every record calls a database; a batching
  window of 1 to 5 seconds for mixed sizes.
- **Cross-language compatibility**: decode into structs whose tags name the
  producer's fields, or normalize them with `kafka.Map`.

### Troubleshooting

When a payload does not deserialize, look at its bytes:

```go
raw, _ := base64.StdEncoding.DecodeString(*rec.Raw.Value)
fmt.Printf("%d bytes: % x\n", len(raw), raw[:min(len(raw), 50)])
```

A first byte `0x00` followed by four bytes is a Confluent header; `0x03` and
a compression byte, a Glue header: see the wire-format section.

## Testing your code

Build the event from JSON, as Lambda sends it, and call the handler that
`kafka.Consumer` returns:

```go
func TestHandler(t *testing.T) {
	payload := `{"eventSource":"aws:kafka","records":{"orders-0":[
		{"topic":"orders","partition":0,"offset":10,"timestamp":1750547462087,
		 "timestampType":"CREATE_TIME","key":"Yy00Mg==",
		 "value":"eyJpZCI6Im8tMSIsInRvdGFsIjoxMi41fQ==","headers":[]}
	]}}`
	var event kafka.Event
	require.NoError(t, json.Unmarshal([]byte(payload), &event))

	handler := kafka.Consumer(handle, kafka.String(), kafka.JSON[Order]())
	processed, err := handler(t.Context(), &event)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}
```

`lambda.NewHandler(handler).Invoke(ctx, payload)` goes through aws-lambda-go's
own decoding, as `lambda.Start` does.

## Differences from Powertools Python

- Keys and values come out typed (`K`, `V`), not as dictionaries, and schemas
  are parsed when the function starts.
- Confluent Protobuf message indexes are decoded as zigzag varints, as the
  specification defines them; Python reads plain varints, which goes wrong as
  soon as a schema has more than one message type. Glue's message index is
  read as a varint of any length, where Python drops exactly one byte.
- The wire-format wrappers cover every format and AWS Glue; Python covers
  Confluent Avro only.
- A null value is a tombstone, not a deserialization error.
- Output serializers apply whenever they are given (Python ignores them for a
  field without a schema type), and their errors match `ErrOutputSerializer`.
- Repeated header keys are all kept and keep their case.
- Each record's own metadata is checked; Python's deserializer cache checks
  later records against the first one's format.
- Python's stateful `ConsumerRecords.record` becomes the `All` iterator.
