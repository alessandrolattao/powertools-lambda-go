package kafka

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// The event sources of the events Lambda delivers.
const (
	// EventSourceMSK is the source of Amazon MSK and MSK Serverless events.
	EventSourceMSK = "aws:kafka"
	// EventSourceSelfManaged is the source of self-managed Apache Kafka events.
	EventSourceSelfManaged = "SelfManagedKafka"
)

// The timestamp types of a record.
const (
	TimestampCreateTime    = "CREATE_TIME"
	TimestampLogAppendTime = "LOG_APPEND_TIME"
)

// Format is the data format of a key or value, as the schema registry
// integration of the event source mapping reports it, and as a Deserializer
// expects it.
type Format string

// The data formats the schema registry integration reports.
const (
	FormatJSON     Format = "JSON"
	FormatAvro     Format = "AVRO"
	FormatProtobuf Format = "PROTOBUF"
)

// Registry is the schema registry a schema ID belongs to.
type Registry int

// The schema registries Lambda integrates with.
const (
	// RegistryConfluent is Confluent Schema Registry (Confluent Cloud or
	// self-managed): schema IDs are numbers.
	RegistryConfluent Registry = iota + 1
	// RegistryGlue is AWS Glue Schema Registry: schema IDs are the UUIDs of
	// schema versions.
	RegistryGlue
)

// glueSchemaIDLength is the length above which a schema ID is taken for a
// Glue schema version UUID, as Powertools Python decides it: Confluent IDs
// are 32-bit numbers, at most 10 digits.
const glueSchemaIDLength = 20

// SchemaMetadata describes the schema a key or value was validated against,
// when the event source mapping integrates with a schema registry.
type SchemaMetadata struct {
	// DataFormat is AVRO, JSON or PROTOBUF.
	DataFormat Format `json:"dataFormat"`
	// SchemaID is the registry's identifier of the schema: a number for
	// Confluent, a schema version UUID for AWS Glue.
	SchemaID string `json:"schemaId"`
}

// Registry reports which registry SchemaID belongs to, from its shape.
func (m *SchemaMetadata) Registry() Registry {
	if len(m.SchemaID) > glueSchemaIDLength {
		return RegistryGlue
	}
	return RegistryConfluent
}

// Event is what Lambda invokes a function with for an Amazon MSK, MSK
// Serverless or self-managed Apache Kafka event source mapping.
//
// It differs from aws-lambda-go's events.KafkaEvent in what Powertools needs:
// the records keep the order of the event, the schema metadata of the
// schema registry integration is there, a null key or value is told apart
// from an empty one, and header bytes are accepted both signed (as Lambda
// writes them) and unsigned.
type Event struct {
	// EventSource is EventSourceMSK or EventSourceSelfManaged.
	EventSource string `json:"eventSource"`
	// EventSourceARN is the ARN of the MSK cluster; self-managed clusters
	// have none.
	EventSourceARN string `json:"eventSourceArn,omitempty"`
	// BootstrapServers is the comma-separated list of brokers.
	BootstrapServers string `json:"bootstrapServers"`
	// Records holds the records grouped by topic partition, in the order of
	// the event.
	Records TopicPartitions `json:"records"`
}

// BootstrapServerList returns the brokers of BootstrapServers, split on
// commas as Powertools Python's decoded_bootstrap_servers does; nil when
// there are none.
func (e *Event) BootstrapServerList() []string {
	if e == nil || e.BootstrapServers == "" {
		return nil
	}
	return strings.Split(e.BootstrapServers, ",")
}

// All returns every record of the event, partition after partition, in the
// order of the event.
func (e *Event) All() iter.Seq[*Record] {
	return func(yield func(*Record) bool) {
		if e == nil {
			return
		}
		for i := range e.Records {
			for j := range e.Records[i].Records {
				if !yield(&e.Records[i].Records[j]) {
					return
				}
			}
		}
	}
}

// Len returns the number of records in the event.
func (e *Event) Len() int {
	if e == nil {
		return 0
	}
	n := 0
	for _, tp := range e.Records {
		n += len(tp.Records)
	}
	return n
}

// TopicPartition is the records of one partition of a topic.
type TopicPartition struct {
	// Name is the name the event files the records under: "<topic>-<partition>".
	Name    string
	Records []Record
}

// TopicPartitions is the "records" object of an event: a JSON object keyed by
// topic partition, read into a slice so that its order survives.
type TopicPartitions []TopicPartition

// UnmarshalJSON reads the "records" object, member by member, in order.
func (p *TopicPartitions) UnmarshalJSON(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil {
		return fmt.Errorf("kafka: records: %w", err)
	}
	switch tok.Kind() {
	case 'n':
		*p = nil
		return nil
	case '{':
	default:
		return fmt.Errorf("kafka: records: %w, found %v", errNotObject, tok.Kind())
	}
	var out TopicPartitions
	for dec.PeekKind() != '}' {
		name, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("kafka: records: %w", err)
		}
		tp := TopicPartition{Name: name.String()}
		if err := json.UnmarshalDecode(dec, &tp.Records); err != nil {
			return fmt.Errorf("kafka: records of %q: %w", tp.Name, err)
		}
		out = append(out, tp)
	}
	if _, err := dec.ReadToken(); err != nil {
		return fmt.Errorf("kafka: records: %w", err)
	}
	*p = out
	return nil
}

// MarshalJSONTo writes the "records" object, in order.
func (p TopicPartitions) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return fmt.Errorf("kafka: records: %w", err)
	}
	for _, tp := range p {
		if err := enc.WriteToken(jsontext.String(tp.Name)); err != nil {
			return fmt.Errorf("kafka: records: %w", err)
		}
		records := tp.Records
		if records == nil {
			records = []Record{}
		}
		if err := json.MarshalEncode(enc, records); err != nil {
			return fmt.Errorf("kafka: records of %q: %w", tp.Name, err)
		}
	}
	if err := enc.WriteToken(jsontext.EndObject); err != nil {
		return fmt.Errorf("kafka: records: %w", err)
	}
	return nil
}

// Record is a Kafka record as the event carries it: key and value encoded in
// base64, header values as byte arrays.
type Record struct {
	Topic     string `json:"topic"`
	Partition int64  `json:"partition"`
	Offset    int64  `json:"offset"`
	// Timestamp is in milliseconds since the Unix epoch; Time converts it.
	Timestamp int64 `json:"timestamp"`
	// TimestampType is TimestampCreateTime or TimestampLogAppendTime.
	TimestampType string `json:"timestampType"`
	// Key is the base64 key, nil when the record has none.
	Key *string `json:"key,omitzero"`
	// Value is the base64 value, nil for a tombstone.
	Value               *string         `json:"value"`
	Headers             Headers         `json:"headers"`
	KeySchemaMetadata   *SchemaMetadata `json:"keySchemaMetadata,omitzero"`
	ValueSchemaMetadata *SchemaMetadata `json:"valueSchemaMetadata,omitzero"`
}

// Time returns Timestamp as a time.
func (r *Record) Time() time.Time {
	return time.UnixMilli(r.Timestamp)
}

// TopicPartition returns "<topic>-<partition>", the name the event files the
// record under and the partition a batch response reports it with.
func (r *Record) TopicPartition() string {
	return r.Topic + "-" + strconv.FormatInt(r.Partition, 10)
}

// Header is a record header. Kafka allows a key to repeat.
type Header struct {
	Key   string
	Value []byte
}

// Headers is the headers of a record, in order. On the wire it is a list of
// objects mapping each key to its value as an array of numbers.
type Headers []Header

// Get returns the value of the last header named name, ignoring case as
// Powertools Python does.
func (h Headers) Get(name string) ([]byte, bool) {
	for _, header := range slices.Backward(h) {
		if strings.EqualFold(header.Key, name) {
			return header.Value, true
		}
	}
	return nil, false
}

// Values returns the values of every header named name, ignoring case, in
// order.
func (h Headers) Values(name string) [][]byte {
	var values [][]byte
	for _, header := range h {
		if strings.EqualFold(header.Key, name) {
			values = append(values, header.Value)
		}
	}
	return values
}

// All returns the headers as key and value pairs, in order, keys spelled as
// the producer spelled them.
func (h Headers) All() iter.Seq2[string, []byte] {
	return func(yield func(string, []byte) bool) {
		for _, header := range h {
			if !yield(header.Key, header.Value) {
				return
			}
		}
	}
}

// UnmarshalJSON reads the headers of a record. Lambda writes every byte as a
// signed number (Java's byte); unsigned numbers are accepted too.
func (h *Headers) UnmarshalJSON(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil {
		return fmt.Errorf("kafka: headers: %w", err)
	}
	switch tok.Kind() {
	case 'n':
		*h = nil
		return nil
	case '[':
	default:
		return fmt.Errorf("kafka: headers: %w, found %v", errNotArray, tok.Kind())
	}
	out := Headers{}
	for dec.PeekKind() != ']' {
		if out, err = readHeaderObject(dec, out); err != nil {
			return fmt.Errorf("kafka: headers: %w", err)
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return fmt.Errorf("kafka: headers: %w", err)
	}
	*h = out
	return nil
}

// readHeaderObject reads one {"key": [bytes...]} object; Lambda writes one
// key per object, every key found is kept.
func readHeaderObject(dec *jsontext.Decoder, out Headers) (Headers, error) {
	tok, err := readToken(dec)
	if err != nil {
		return out, err
	}
	if tok.Kind() != '{' {
		return out, fmt.Errorf("%w, found %v", errNotObject, tok.Kind())
	}
	for dec.PeekKind() != '}' {
		if tok, err = readToken(dec); err != nil {
			return out, err
		}
		header := Header{Key: tok.String()}
		if header.Value, err = readHeaderBytes(dec); err != nil {
			return out, fmt.Errorf("header %q: %w", header.Key, err)
		}
		out = append(out, header)
	}
	_, err = readToken(dec)
	return out, err
}

func readHeaderBytes(dec *jsontext.Decoder) ([]byte, error) {
	start, startErr := readToken(dec)
	if startErr != nil {
		return nil, startErr
	}
	switch start.Kind() {
	case 'n':
		return nil, nil
	case '[':
	default:
		return nil, fmt.Errorf("%w, found %v", errNotArray, start.Kind())
	}
	value := []byte{}
	for dec.PeekKind() != ']' {
		tok, err := readToken(dec)
		if err != nil {
			return nil, err
		}
		if tok.Kind() != '0' {
			return nil, fmt.Errorf("%w, found %v", errNotByte, tok.Kind())
		}
		if n, intErr := tok.Int(); intErr == nil && n >= -128 && n <= 255 {
			value = append(value, byte(n&0xFF))
			continue
		}
		return nil, fmt.Errorf("%w: %s", errNotByte, tok.String())
	}
	_, err := readToken(dec)
	return value, err
}

// readToken reads the next token of dec.
func readToken(dec *jsontext.Decoder) (jsontext.Token, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return tok, fmt.Errorf("json: %w", err)
	}
	return tok, nil
}

// MarshalJSONTo writes the headers the way Lambda does: one object per
// header, bytes as signed numbers.
func (h Headers) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return fmt.Errorf("kafka: headers: %w", err)
	}
	for _, header := range h {
		if err := writeHeader(enc, header); err != nil {
			return fmt.Errorf("kafka: header %q: %w", header.Key, err)
		}
	}
	if err := enc.WriteToken(jsontext.EndArray); err != nil {
		return fmt.Errorf("kafka: headers: %w", err)
	}
	return nil
}

func writeHeader(enc *jsontext.Encoder, header Header) error {
	tokens := make([]jsontext.Token, 0, len(header.Value)+5)
	tokens = append(tokens, jsontext.BeginObject, jsontext.String(header.Key), jsontext.BeginArray)
	for _, b := range header.Value {
		// Java's signed byte: 0x80 to 0xFF are -128 to -1.
		n := int64(b)
		if n > 127 {
			n -= 256
		}
		tokens = append(tokens, jsontext.Int(n))
	}
	tokens = append(tokens, jsontext.EndArray, jsontext.EndObject)
	for _, tok := range tokens {
		if err := enc.WriteToken(tok); err != nil {
			return fmt.Errorf("json: %w", err)
		}
	}
	return nil
}

var (
	errNotObject = errors.New("expected a JSON object")
	errNotArray  = errors.New("expected a JSON array")
	errNotByte   = errors.New("expected a byte between -128 and 255")
)

// FromLambdaEvent converts aws-lambda-go's Kafka event, for code that already
// decoded one. What events.KafkaEvent does not keep cannot be recovered: the
// partitions come sorted by name rather than in the order of the event, the
// schema metadata is missing, and an empty value is taken for a tombstone.
func FromLambdaEvent(event events.KafkaEvent) *Event {
	out := &Event{
		EventSource:      event.EventSource,
		EventSourceARN:   event.EventSourceARN,
		BootstrapServers: event.BootstrapServers,
	}
	for _, name := range slices.Sorted(maps.Keys(event.Records)) {
		tp := TopicPartition{Name: name, Records: make([]Record, 0, len(event.Records[name]))}
		for i := range event.Records[name] {
			tp.Records = append(tp.Records, FromLambdaRecord(event.Records[name][i]))
		}
		out.Records = append(out.Records, tp)
	}
	return out
}

// FromLambdaRecord converts one record of aws-lambda-go's Kafka event, with
// the losses FromLambdaEvent describes.
func FromLambdaRecord(record events.KafkaRecord) Record {
	out := Record{
		Topic:         record.Topic,
		Partition:     record.Partition,
		Offset:        record.Offset,
		Timestamp:     record.Timestamp.UnixMilli(),
		TimestampType: record.TimestampType,
		Headers:       Headers{},
	}
	if record.Key != "" {
		out.Key = new(record.Key)
	}
	if record.Value != "" {
		out.Value = new(record.Value)
	}
	for _, headers := range record.Headers {
		for _, key := range slices.Sorted(maps.Keys(headers)) {
			out.Headers = append(out.Headers, Header{Key: key, Value: []byte(headers[key])})
		}
	}
	return out
}
