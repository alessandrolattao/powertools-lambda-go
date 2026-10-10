package batch_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/batch"
)

func ExampleProcessSQS() {
	handle := func(_ context.Context, msg events.SQSMessage) error {
		if msg.Body == "" {
			return errors.New("empty message")
		}
		return nil // processed: SQS deletes the message
	}

	event := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "059f36b4", Body: "hello"},
		{MessageId: "244fc6b4", Body: ""},
	}}
	resp, err := batch.ProcessSQS(context.Background(), event, handle)
	if err != nil {
		panic(err)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"batchItemFailures":[{"itemIdentifier":"244fc6b4"}]}
}

func ExampleHandler() {
	handle := func(_ context.Context, rec events.KinesisEventRecord) error {
		fmt.Printf("%s\n", rec.Kinesis.Data)
		return nil
	}

	// In main: lambda.Start(handler)
	handler := batch.Handler(batch.ProcessKinesis, handle)

	event := events.KinesisEvent{Records: []events.KinesisEventRecord{
		{Kinesis: events.KinesisRecord{SequenceNumber: "4950", Data: []byte("hello")}},
	}}
	resp, err := handler(context.Background(), event)
	fmt.Println(len(resp.BatchItemFailures), err)
	// Output:
	// hello
	// 0 <nil>
}

func ExampleSQSJSON() {
	type Order struct {
		Item string `json:"item"`
	}
	handle := func(_ context.Context, order Order) error {
		fmt.Println("ordered", order.Item)
		return nil
	}

	event := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "1", Body: `{"item":"coffee"}`},
		{MessageId: "2", Body: `not an order`},
	}}
	resp, err := batch.ProcessSQS(context.Background(), event, batch.SQSJSON(handle))
	if err != nil {
		panic(err)
	}
	fmt.Println("failed:", resp.BatchItemFailures[0].ItemIdentifier)
	// Output:
	// ordered coffee
	// failed: 2
}

func ExampleProcessSQSFIFO() {
	handle := func(_ context.Context, msg events.SQSMessage) error {
		if msg.Body == "bad" {
			return errors.New("cannot process")
		}
		fmt.Println("processed", msg.MessageId)
		return nil
	}

	group := func(id string) map[string]string { return map[string]string{"MessageGroupId": id} }
	event := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "1", Body: "bad", Attributes: group("customer-a")},
		{MessageId: "2", Body: "ok", Attributes: group("customer-b")},
		{MessageId: "3", Body: "ok", Attributes: group("customer-a")},
	}}
	resp, err := batch.ProcessSQSFIFO(context.Background(), event, handle, batch.WithSkipGroupOnError())
	if err != nil {
		panic(err)
	}
	for _, f := range resp.BatchItemFailures {
		fmt.Println("retry", f.ItemIdentifier)
	}
	// Output:
	// processed 2
	// retry 1
	// retry 3
}

func ExampleProcessKafka() {
	handle := func(_ context.Context, rec events.KafkaRecord) error {
		if rec.Offset == 16 {
			return errors.New("poison pill")
		}
		return nil
	}

	event := events.KafkaEvent{Records: map[string][]events.KafkaRecord{
		"mytopic-0": {{Topic: "mytopic", Partition: 0, Offset: 15}, {Topic: "mytopic", Partition: 0, Offset: 16}},
	}}
	resp, err := batch.ProcessKafka(context.Background(), event, handle)
	if err != nil {
		panic(err)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"batchItemFailures":[{"itemIdentifier":{"partition":"mytopic-0","offset":16}}]}
}

func ExampleDynamoDBImages() {
	type Product struct {
		SKU   string `json:"sku"`
		Stock int    `json:"stock"`
	}
	handle := func(_ context.Context, newImage, oldImage *Product) error {
		switch {
		case oldImage == nil:
			fmt.Println("new product", newImage.SKU, newImage.Stock)
		case newImage == nil:
			fmt.Println("removed product", oldImage.SKU)
		default:
			fmt.Println("stock of", newImage.SKU, "went from", oldImage.Stock, "to", newImage.Stock)
		}
		return nil
	}

	image := func(stock string) map[string]events.DynamoDBAttributeValue {
		return map[string]events.DynamoDBAttributeValue{
			"sku":   events.NewStringAttribute("sku-1"),
			"stock": events.NewNumberAttribute(stock),
		}
	}
	event := events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{
		{Change: events.DynamoDBStreamRecord{SequenceNumber: "1", NewImage: image("5")}},
		{Change: events.DynamoDBStreamRecord{SequenceNumber: "2", NewImage: image("3"), OldImage: image("5")}},
		{Change: events.DynamoDBStreamRecord{SequenceNumber: "3", OldImage: image("3")}},
	}}
	if _, err := batch.ProcessDynamoDB(context.Background(), event, batch.DynamoDBImages(handle)); err != nil {
		panic(err)
	}
	// Output:
	// new product sku-1 5
	// stock of sku-1 went from 5 to 3
	// removed product sku-1
}

// A custom processor in Powertools Python stores the successful records
// once the batch is done; here a hook collects them, safely even when the
// records run concurrently.
func ExampleWithAfterRecord() {
	var (
		mu        sync.Mutex
		succeeded []string
	)
	collect := batch.WithAfterRecord(func(_ context.Context, msg events.SQSMessage, err error) {
		if err == nil {
			mu.Lock()
			defer mu.Unlock()
			succeeded = append(succeeded, msg.MessageId)
		}
	})

	event := events.SQSEvent{Records: []events.SQSMessage{{MessageId: "a"}, {MessageId: "b"}}}
	_, err := batch.ProcessSQS(context.Background(), event,
		func(context.Context, events.SQSMessage) error { return nil },
		collect, batch.WithConcurrency(2))
	if err != nil {
		panic(err)
	}
	// Write them all at once, as the custom processor does in _clean.
	fmt.Println(len(succeeded), "records to store")
	// Output: 2 records to store
}

func ExampleWithRaiseOnEntireBatchFailure() {
	failing := func(context.Context, events.SQSMessage) error { return errors.New("downstream unavailable") }
	event := events.SQSEvent{Records: []events.SQSMessage{{MessageId: "a"}}}

	_, err := batch.ProcessSQS(context.Background(), event, failing)
	batchFailure, ok := errors.AsType[*batch.ProcessingError](err)
	fmt.Println("default:", ok, len(batchFailure.Errors))

	resp, err := batch.ProcessSQS(context.Background(), event, failing, batch.WithRaiseOnEntireBatchFailure(false))
	fmt.Println("disabled:", err, len(resp.BatchItemFailures))
	// Output:
	// default: true 1
	// disabled: <nil> 1
}
