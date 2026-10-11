// Package protobuf deserializes Kafka keys and values written in Protocol
// Buffers: Powertools Python's value_schema_type="PROTOBUF" (and
// key_schema_type).
//
// New reads messages of a generated type; Dynamic reads messages described
// by a descriptor only, for schemas without generated code:
//
//	var users = protobuf.New[*userpb.User]()
//
//	lambda.Start(kafka.Consumer(handle, kafka.String(), users))
//
// The framing of the payload follows the record's schema metadata, as in
// Powertools Python:
//
//   - no metadata: a plain message;
//   - a Confluent schema ID (a number): the message indexes Confluent's
//     serializer writes before the message are skipped;
//   - an AWS Glue schema version ID (a UUID): the message index Glue's
//     serializer writes before the message is skipped.
//
// Python reads Confluent message indexes as plain varints and drops exactly
// one byte for Glue; this package reads the zigzag varints the Confluent
// specification defines, and Glue's varint index whatever its length. The
// indexes locate the message type in the .proto file: the deserializer
// always reads its own type, as Python does.
//
// Payloads that still carry the registry header, because the event source
// mapping does not integrate with the registry, are read by wrapping the
// deserializer with kafka.ConfluentWireFormat or kafka.GlueWireFormat.
//
// Python returns a dictionary (MessageToDict with the .proto field names);
// Go returns the message. AsMap gives the dictionary when it is wanted.
package protobuf

import (
	"encoding/json/v2"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/kafka"
)

// New returns a deserializer of messages of the generated type M, a pointer
// such as *userpb.User. Use Dynamic for dynamicpb messages.
func New[M proto.Message]() kafka.Deserializer[M] {
	var zero M
	if _, dynamic := any(zero).(*dynamicpb.Message); dynamic {
		panic("protobuf: New cannot build dynamicpb messages, use Dynamic")
	}
	mt := zero.ProtoReflect().Type()
	return deserializer[M]{
		newMessage: func() (M, error) {
			m, ok := mt.New().Interface().(M)
			if !ok {
				return m, fmt.Errorf("%w: protobuf: %s does not build a %T", kafka.ErrDeserialization, mt.Descriptor().FullName(), zero)
			}
			return m, nil
		},
	}
}

// Dynamic returns a deserializer of messages described by desc, for
// schemas known only at run time (from a FileDescriptorSet, for one).
func Dynamic(desc protoreflect.MessageDescriptor) kafka.Deserializer[*dynamicpb.Message] {
	return deserializer[*dynamicpb.Message]{
		newMessage: func() (*dynamicpb.Message, error) {
			return dynamicpb.NewMessage(desc), nil
		},
	}
}

type deserializer[M proto.Message] struct {
	newMessage func() (M, error)
}

func (deserializer[M]) Format() kafka.Format { return kafka.FormatProtobuf }

func (d deserializer[M]) Deserialize(data []byte, meta *kafka.SchemaMetadata) (M, error) {
	debug.Logger().Debug("deserializing data with PROTOBUF format")
	m, err := d.newMessage()
	if err != nil {
		return m, err
	}
	payload, err := StripMessageIndexes(data, meta)
	if err != nil {
		var zero M
		return zero, err
	}
	if err := proto.Unmarshal(payload, m); err != nil {
		var zero M
		return zero, fmt.Errorf("%w: protobuf: %w", kafka.ErrDeserialization, err)
	}
	return m, nil
}

// StripMessageIndexes returns the message in data, removing the message
// indexes a schema registry serializer writes before it, as meta tells:
// none without metadata, Confluent's zigzag varint list for a numeric schema
// ID, Glue's single varint for a schema version UUID.
func StripMessageIndexes(data []byte, meta *kafka.SchemaMetadata) ([]byte, error) {
	switch {
	case meta == nil || meta.SchemaID == "":
		debug.Logger().Debug("plain PROTOBUF data")
		return data, nil
	case meta.Registry() == kafka.RegistryGlue:
		debug.Logger().Debug("PROTOBUF data framed by the Glue schema registry")
		_, n := protowire.ConsumeVarint(data)
		if n < 0 {
			return nil, fmt.Errorf("%w: truncated Glue message index", kafka.ErrInvalidWireFormat)
		}
		return data[n:], nil
	default:
		debug.Logger().Debug("PROTOBUF data framed by the Confluent schema registry")
		_, rest, err := kafka.ParseConfluentMessageIndexes(data)
		return rest, err
	}
}

// AsMap turns d into a deserializer of the dictionary Powertools Python
// returns: the protobuf JSON mapping of the message with the .proto field
// names (MessageToDict with preserving_proto_field_name=True), so 64-bit
// integers are strings, enums are names and bytes are base64.
func AsMap[M proto.Message](d kafka.Deserializer[M]) kafka.Deserializer[map[string]any] {
	return kafka.Map(d, func(m M) (map[string]any, error) {
		raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("protobuf: to JSON: %w", err)
		}
		out := map[string]any{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("protobuf: from JSON: %w", err)
		}
		return out, nil
	})
}
