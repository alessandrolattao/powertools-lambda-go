package kafka_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
)

// sampleEvent is an MSK event with two records, as Lambda delivers it.
const sampleEvent = `{
	"eventSource": "aws:kafka",
	"eventSourceArn": "arn:aws:kafka:eu-west-1:123456789012:cluster/orders/abcd",
	"bootstrapServers": "b-1.orders.kafka.eu-west-1.amazonaws.com:9098",
	"records": {
		"orders-0": [
			{
				"topic": "orders", "partition": 0, "offset": 10, "timestamp": 1750547462087,
				"timestampType": "CREATE_TIME", "key": "Yy00Mg==",
				"value": "eyJpZCI6Im8tMSIsInRvdGFsIjoxMi41fQ==",
				"headers": [{"correlation-id": [97, 98, 99]}]
			},
			{
				"topic": "orders", "partition": 0, "offset": 11, "timestamp": 1750547462088,
				"timestampType": "CREATE_TIME", "key": "Yy03",
				"value": "bm90IGpzb24=",
				"headers": []
			}
		]
	}
}`

type Order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

func ExampleConsumer() {
	handle := func(_ context.Context, records *kafka.ConsumerRecords[string, Order]) (int, error) {
		processed := 0
		for rec, err := range records.All() {
			if errors.Is(err, kafka.ErrDeserialization) {
				// Send rec.Raw to a dead-letter queue and go on.
				fmt.Println("skipped offset", rec.Offset)
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

	// In main: lambda.Start(handler).
	handler := kafka.Consumer(handle, kafka.String(), kafka.JSON[Order]())

	var event kafka.Event
	if err := json.Unmarshal([]byte(sampleEvent), &event); err != nil {
		panic(err)
	}
	processed, err := handler(context.Background(), &event)
	fmt.Println(processed, err)
	// Output:
	// customer c-42 ordered o-1 for 12.50 (abc)
	// skipped offset 11
	// 1 <nil>
}

func ExampleNewConsumerRecords() {
	var event kafka.Event
	if err := json.Unmarshal([]byte(sampleEvent), &event); err != nil {
		panic(err)
	}

	// Values read as text, as Powertools Python does without a schema.
	records := kafka.NewConsumerRecords(&event, kafka.String(), kafka.String())
	for rec, err := range records.All() {
		if err != nil {
			panic(err)
		}
		fmt.Println(rec.Raw.TopicPartition(), rec.Offset, rec.Value)
	}
	// Output:
	// orders-0 10 {"id":"o-1","total":12.5}
	// orders-0 11 not json
}

func ExampleMap() {
	// An output serializer: normalize what the producer wrote into the
	// function's own type.
	upper := kafka.Map(kafka.String(), func(s string) (string, error) {
		return strings.ToUpper(s), nil
	})
	value, err := upper.Deserialize([]byte("hello"), nil)
	fmt.Println(value, err)
	// Output: HELLO <nil>
}

func ExampleConfluentWireFormat() {
	// A payload written by Confluent's KafkaJsonSchemaSerializer: magic
	// byte, schema ID 7, then the document.
	payload := append([]byte{0, 0, 0, 0, 7}, `{"id":"o-2","total":3}`...)

	orders := kafka.ConfluentWireFormat(kafka.JSON[Order]())
	order, err := orders.Deserialize(payload, nil)
	fmt.Println(order.ID, err)
	// Output: o-2 <nil>
}
