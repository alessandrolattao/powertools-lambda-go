package parser

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// DynamoDBImages holds the images of a DynamoDB stream record, decoded into
// a T: Python's {"NewImage": ..., "OldImage": ...}. An image the record does
// not carry (it depends on the stream view type and on the operation) is
// nil.
type DynamoDBImages[T any] struct {
	NewImage *T `json:"NewImage"`
	OldImage *T `json:"OldImage"`
}

// Validate validates the images that are there.
func (d DynamoDBImages[T]) Validate() error {
	if d.NewImage != nil {
		if err := validate(d.NewImage); err != nil {
			return fmt.Errorf("NewImage: %w", err)
		}
	}
	if d.OldImage != nil {
		if err := validate(d.OldImage); err != nil {
			return fmt.Errorf("OldImage: %w", err)
		}
	}
	return nil
}

// DynamoDBStreamEnvelope unwraps the images of every record of a DynamoDB
// stream event, Python's DynamoDBStreamEnvelope: the payload of a record is
// {"NewImage": ..., "OldImage": ...}, so the model to parse into is a slice
// of DynamoDBImages. Attribute values become plain JSON (see
// eventsource.DynamoDBImageJSON): numbers keep every digit, binary values
// are base64 strings.
var DynamoDBStreamEnvelope Envelope = dynamoDBStream

var dynamoDBStream = envelope[events.DynamoDBEvent]{
	batch: true,
	extract: func(e *events.DynamoDBEvent) ([]payload, error) {
		out := make([]payload, 0, len(e.Records))
		for i := range e.Records {
			source := "Records[" + strconv.Itoa(i) + "].dynamodb"
			newImage, err := imageJSON(e.Records[i].Change.NewImage, source+".NewImage")
			if err != nil {
				return nil, err
			}
			oldImage, err := imageJSON(e.Records[i].Change.OldImage, source+".OldImage")
			if err != nil {
				return nil, err
			}
			out = append(out, payload{
				data:   []byte(`{"NewImage":` + string(newImage) + `,"OldImage":` + string(oldImage) + `}`),
				source: source,
			})
		}
		return out, nil
	},
}

func imageJSON(image map[string]events.DynamoDBAttributeValue, source string) (jsontext.Value, error) {
	if image == nil {
		return jsontext.Value("null"), nil
	}
	raw, err := eventsource.DynamoDBImageJSON(image)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidPayload, source, err)
	}
	return raw, nil
}

// DynamoDBStream decodes the images of every record of a DynamoDB stream
// event into a T. The result has one element per record.
func DynamoDBStream[T any](event events.DynamoDBEvent, opts ...Option) ([]DynamoDBImages[T], error) {
	cfg := newConfig(opts)
	out := make([]DynamoDBImages[T], 0, len(event.Records))
	for i := range event.Records {
		source := "Records[" + strconv.Itoa(i) + "].dynamodb"
		newImage, err := decodeImage[T](event.Records[i].Change.NewImage, source+".NewImage", cfg.json)
		if err != nil {
			return nil, err
		}
		oldImage, err := decodeImage[T](event.Records[i].Change.OldImage, source+".OldImage", cfg.json)
		if err != nil {
			return nil, err
		}
		out = append(out, DynamoDBImages[T]{NewImage: newImage, OldImage: oldImage})
	}
	return out, nil
}

func decodeImage[T any](image map[string]events.DynamoDBAttributeValue, source string, opts []json.Options) (*T, error) {
	if image == nil {
		return nil, nil //nolint:nilnil // a record without this image has none to decode
	}
	raw, err := imageJSON(image, source)
	if err != nil {
		return nil, parseError[T](source, err)
	}
	v, err := decode[T](raw, source, opts)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// KafkaEnvelope unwraps the value of every record of an Amazon MSK or
// self-managed Apache Kafka event, Python's KafkaEnvelope: base64-decoded,
// then parsed as JSON. Records come in the order of the event, topic
// partition after topic partition. A record without value (a tombstone) has
// no payload.
var KafkaEnvelope Envelope = kafkaEnvelope{}

type kafkaEnvelope struct{}

// kafkaRecord is the part of a Kafka record the envelope reads.
type kafkaRecord struct {
	Value *string `json:"value"`
}

// Unwrap reads the records object member by member, so that the payloads
// keep the order of the event, which a Go map would lose.
func (kafkaEnvelope) Unwrap(raw jsontext.Value) (jsontext.Value, error) {
	var event struct {
		Records jsontext.Value `json:"records"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("%w (Kafka): %w", ErrInvalidEnvelope, err)
	}
	if len(event.Records) == 0 || event.Records.Kind() != '{' {
		return payloadArray(nil)
	}
	dec := jsontext.NewDecoder(bytes.NewReader(event.Records))
	if _, err := dec.ReadToken(); err != nil {
		return nil, fmt.Errorf("%w (Kafka): %w", ErrInvalidEnvelope, err)
	}
	var payloads []payload
	for dec.PeekKind() != '}' {
		partition, err := kafkaPartition(dec)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, partition...)
	}
	return payloadArray(payloads)
}

// kafkaPartition reads the next member of the records object: the records
// of one topic partition.
func kafkaPartition(dec *jsontext.Decoder) ([]payload, error) {
	token, err := dec.ReadToken()
	if err != nil {
		return nil, fmt.Errorf("%w (Kafka): %w", ErrInvalidEnvelope, err)
	}
	// The token is only valid until the next read.
	name := token.String()
	var records []kafkaRecord
	if err := json.UnmarshalDecode(dec, &records); err != nil {
		return nil, fmt.Errorf("%w (Kafka): records[%q]: %w", ErrInvalidEnvelope, name, err)
	}
	payloads := make([]payload, 0, len(records))
	for i, record := range records {
		p, err := kafkaPayload(record.Value, "records["+strconv.Quote(name)+"]["+strconv.Itoa(i)+"].value")
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
	}
	return payloads, nil
}

func kafkaPayload(value *string, source string) (payload, error) {
	if value == nil {
		return payload{source: source}, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(*value)
	if err != nil {
		return payload{}, fmt.Errorf("%w: %s is not base64: %w", ErrInvalidPayload, source, err)
	}
	return payload{data: decoded, source: source}, nil
}

// Kafka decodes the value of every record of a Kafka event into a T.
// events.KafkaEvent holds the records in a map, which has no order: the
// records come sorted by topic partition, then in their order within it.
// Parse with KafkaEnvelope from the raw event to keep the event's order.
func Kafka[T any](event events.KafkaEvent, opts ...Option) ([]T, error) {
	keys := slices.Sorted(maps.Keys(event.Records))
	var payloads []payload
	for _, key := range keys {
		records := event.Records[key]
		for i := range records {
			source := "records[" + strconv.Quote(key) + "][" + strconv.Itoa(i) + "].value"
			if records[i].Value == "" {
				payloads = append(payloads, payload{source: source})
				continue
			}
			p, err := kafkaPayload(&records[i].Value, source)
			if err != nil {
				return nil, parseError[T]("", err)
			}
			payloads = append(payloads, p)
		}
	}
	return decodeAll[T](payloads, newConfig(opts).json)
}
