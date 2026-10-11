package avro_test

import (
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
	"github.com/alessandrolattao/powertools-lambda-go/kafka/avro"
)

func ExampleNew() {
	// Parse the schema once, at package level: an invalid schema stops the
	// function at startup.
	users := kafka.Must(avro.New[User](userSchema))

	// The value of a record, base64-decoded: "Powertools", 5.
	value := []byte{0x14, 'P', 'o', 'w', 'e', 'r', 't', 'o', 'o', 'l', 's', 0x0a}
	user, err := users.Deserialize(value, nil)
	fmt.Println(user.Name, user.Age, err)
	// Output: Powertools 5 <nil>
}

func ExampleNew_confluentWireFormat() {
	// A payload of Confluent's KafkaAvroSerializer, read without the schema
	// registry integration: Python's value_schema_wire_format="CONFLUENT".
	users := kafka.ConfluentWireFormat(kafka.Must(avro.New[map[string]any](userSchema)))

	value := []byte{0x00, 0, 0, 0, 42, 0x06, 'a', 'n', 'n', 0x0e}
	user, err := users.Deserialize(value, nil)
	fmt.Println(user["name"], user["age"], err)
	// Output: ann 7 <nil>
}
