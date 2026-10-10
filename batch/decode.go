package batch

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

// ErrDecode fails a record whose payload does not decode into the handler's
// type, or decodes into a value whose Validate method rejects it: a poison
// pill, reported like any other failed record.
var ErrDecode = errors.New("decode the record payload")

// validator is a decoded payload that checks itself, as the parser package
// does with the types it decodes.
type validator interface {
	Validate() error
}

// SQSJSON adapts handler, which takes the message body decoded from JSON
// into a T, to a record handler for ProcessSQS and ProcessSQSFIFO: the
// Powertools Python Pydantic integration, with a Go struct as the model.
//
// A body that is not JSON, or does not fit T, fails the message with
// ErrDecode. Decoding is lenient: unknown fields are ignored, missing ones
// stay zero. When T (or *T) has a Validate() error method, it runs after
// decoding and its error fails the message too.
func SQSJSON[T any](handler func(ctx context.Context, body T) error) RecordHandler[events.SQSMessage] {
	return func(ctx context.Context, msg events.SQSMessage) error {
		body, err := decodeJSON[T]([]byte(msg.Body))
		if err != nil {
			return err
		}
		return handler(ctx, body)
	}
}

// KinesisJSON adapts handler, which takes the record's data decoded from
// JSON into a T, to a record handler for ProcessKinesis. Decoding is as for
// SQSJSON; aws-lambda-go has already decoded the data from base64.
func KinesisJSON[T any](handler func(ctx context.Context, data T) error) RecordHandler[events.KinesisEventRecord] {
	return func(ctx context.Context, rec events.KinesisEventRecord) error {
		data, err := decodeJSON[T](rec.Kinesis.Data)
		if err != nil {
			return err
		}
		return handler(ctx, data)
	}
}

// KafkaJSON adapts handler, which takes the record's value decoded from
// base64 and then from JSON into a T, to a record handler for ProcessKafka.
// Decoding is as for SQSJSON. For Avro and Protobuf values, see the kafka
// package.
func KafkaJSON[T any](handler func(ctx context.Context, value T) error) RecordHandler[events.KafkaRecord] {
	return func(ctx context.Context, rec events.KafkaRecord) error {
		raw, err := base64.StdEncoding.DecodeString(rec.Value)
		if err != nil {
			return fmt.Errorf("%w: the value is not base64: %w", ErrDecode, err)
		}
		value, err := decodeJSON[T](raw)
		if err != nil {
			return err
		}
		return handler(ctx, value)
	}
}

// DynamoDBImages adapts handler, which takes the item images of a stream
// record decoded into a T, to a record handler for ProcessDynamoDB.
//
// An image is nil when the record lacks it: there is no new image in a
// REMOVE record, no old one in an INSERT, and the stream's view type
// decides which images the others carry. Attribute values become JSON
// first, the way Powertools Python's TypeDeserializer reads them (numbers
// as numbers, binary as base64 strings, sets as arrays), then decode into T
// as for SQSJSON.
func DynamoDBImages[T any](handler func(ctx context.Context, newImage, oldImage *T) error) RecordHandler[events.DynamoDBEventRecord] {
	return func(ctx context.Context, rec events.DynamoDBEventRecord) error {
		newImage, err := decodeImage[T](rec.Change.NewImage)
		if err != nil {
			return fmt.Errorf("new image: %w", err)
		}
		oldImage, err := decodeImage[T](rec.Change.OldImage)
		if err != nil {
			return fmt.Errorf("old image: %w", err)
		}
		return handler(ctx, newImage, oldImage)
	}
}

func decodeJSON[T any](data []byte) (T, error) {
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("%w into %T: %w", ErrDecode, out, err)
	}
	if v, ok := any(&out).(validator); ok {
		if err := v.Validate(); err != nil {
			return out, fmt.Errorf("%w: invalid %T: %w", ErrDecode, out, err)
		}
	}
	return out, nil
}

func decodeImage[T any](image map[string]events.DynamoDBAttributeValue) (*T, error) {
	if len(image) == 0 {
		return nil, nil //nolint:nilnil // a missing image is not an error, and nil is how the handler learns of it
	}
	raw, err := imageJSON(image)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecode, err)
	}
	out, err := decodeJSON[T](raw)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Errors of the conversion of DynamoDB images to JSON.
var (
	errNotNumber       = errors.New("not a number")
	errUnsupportedType = errors.New("unsupported DynamoDB data type")
)

// imageJSON turns a DynamoDB item into the JSON object it stands for.
func imageJSON(image map[string]events.DynamoDBAttributeValue) ([]byte, error) {
	value, err := attributeValue(events.NewMapAttribute(image))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("marshal the image: %w", err)
	}
	return raw, nil
}

// attributeValue turns a DynamoDB attribute into the value json.Marshal
// writes as its JSON form. Numbers stay as DynamoDB wrote them, so that no
// precision is lost on the way to the target type; binary values become
// base64 strings, as json.Marshal writes byte slices.
func attributeValue(av events.DynamoDBAttributeValue) (any, error) {
	switch av.DataType() {
	case events.DataTypeString:
		return av.String(), nil
	case events.DataTypeNumber:
		return number(av.Number())
	case events.DataTypeBinary:
		return av.Binary(), nil
	case events.DataTypeBoolean:
		return av.Boolean(), nil
	case events.DataTypeNull:
		return jsontext.Value("null"), nil
	case events.DataTypeList:
		return convertAll(av.List(), attributeValue)
	case events.DataTypeMap:
		out := make(map[string]any, len(av.Map()))
		for name, v := range av.Map() {
			converted, err := attributeValue(v)
			if err != nil {
				return nil, err
			}
			out[name] = converted
		}
		return out, nil
	case events.DataTypeStringSet:
		return av.StringSet(), nil
	case events.DataTypeNumberSet:
		return convertAll(av.NumberSet(), number)
	case events.DataTypeBinarySet:
		return av.BinarySet(), nil
	default:
		return nil, fmt.Errorf("%w: %d", errUnsupportedType, av.DataType())
	}
}

// number checks that a DynamoDB number is a JSON number.
func number(n string) (any, error) {
	v := jsontext.Value(n)
	if !v.IsValid() || v.Kind() != '0' {
		return nil, fmt.Errorf("DynamoDB number %q: %w", n, errNotNumber)
	}
	return v, nil
}

func convertAll[V any](values []V, convert func(V) (any, error)) ([]any, error) {
	out := make([]any, 0, len(values))
	for _, v := range values {
		converted, err := convert(v)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}
