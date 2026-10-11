package protobuf_test

import (
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/protobuf"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/protobuf/internal/testpb"
)

func ExampleNew() {
	// A generated message type; in main:
	// lambda.Start(kafka.Consumer(handle, kafka.String(), users)).
	users := protobuf.New[*testpb.User]()

	// The value of a record, base64-decoded: name "powertools", age 5.
	value := []byte{0x0a, 0x0a, 'p', 'o', 'w', 'e', 'r', 't', 'o', 'o', 'l', 's', 0x10, 0x05}
	user, err := users.Deserialize(value, nil)
	fmt.Println(user.GetName(), user.GetAge(), err)

	// The same message from Confluent's serializer, through the schema
	// registry integration: the [0] message index precedes it.
	meta := &kafka.SchemaMetadata{DataFormat: kafka.FormatProtobuf, SchemaID: "42"}
	user, err = users.Deserialize(append([]byte{0x00}, value...), meta)
	fmt.Println(user.GetName(), user.GetAge(), err)
	// Output:
	// powertools 5 <nil>
	// powertools 5 <nil>
}

func ExampleAsMap() {
	// The dictionary Powertools Python returns, with the .proto field names.
	keys := protobuf.AsMap(protobuf.New[*testpb.Key]())

	key, err := keys.Deserialize([]byte{0x1a, 0x03, 'u', '-', '1'}, nil)
	fmt.Println(key, err)
	// Output: map[user_id:u-1] <nil>
}
