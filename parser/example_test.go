package parser_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/parser"
)

type Order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

func (o Order) Validate() error {
	if o.ID == "" {
		return errors.New("id is required")
	}
	return nil
}

func ExampleParse() {
	order, err := parser.Parse[Order](`{"id":"o-1","total":12.5}`)
	if err != nil {
		panic(err)
	}
	fmt.Println(order.ID, order.Total)

	_, err = parser.Parse[Order](`{"total":3}`)
	fmt.Println(err)
	// Output:
	// o-1 12.5
	// parse parser_test.Order: id is required
}

func ExampleSQS() {
	event := events.SQSEvent{Records: []events.SQSMessage{
		{Body: `{"id":"o-1","total":1}`},
		{Body: `{"id":"o-2","total":2}`},
	}}

	orders, err := parser.SQS[Order](event)
	if err != nil {
		panic(err)
	}
	fmt.Println(orders)
	// Output: [{o-1 1} {o-2 2}]
}

func ExampleEventBridge() {
	event := events.EventBridgeEvent{DetailType: "OrderPlaced", Detail: jsontext.Value(`{"id":"o-1","total":9.9}`)}

	order, err := parser.EventBridge[Order](event)
	if err != nil {
		panic(err)
	}
	fmt.Println(order.ID)
	// Output: o-1
}

func ExampleEventParser() {
	handle := func(_ context.Context, orders []Order) (int, error) {
		return len(orders), nil
	}
	handler := parser.EventParser(handle, parser.WithEnvelope(parser.SQSEnvelope))

	// lambda.Start(handler) passes the raw event.
	n, err := handler(context.Background(), jsontext.Value(`{"Records":[{"body":"{\"id\":\"o-1\"}"}]}`))
	if err != nil {
		panic(err)
	}
	fmt.Println(n)
	// Output: 1
}

func ExampleJSON() {
	type OrderMessage struct {
		events.SQSMessage

		Body parser.JSON[Order] `json:"body"`
	}

	message, err := parser.Parse[OrderMessage](`{"messageId":"m-1","body":"{\"id\":\"o-1\",\"total\":3}"}`)
	if err != nil {
		panic(err)
	}
	fmt.Println(message.MessageId, message.Body.Value.ID)
	// Output: m-1 o-1
}

func ExampleDynamoDBStream() {
	event := events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{{
		Change: events.DynamoDBStreamRecord{NewImage: map[string]events.DynamoDBAttributeValue{
			"id":    events.NewStringAttribute("o-1"),
			"total": events.NewNumberAttribute("12.5"),
		}},
	}}}

	records, err := parser.DynamoDBStream[Order](event)
	if err != nil {
		panic(err)
	}
	fmt.Println(*records[0].NewImage, records[0].OldImage == nil)
	// Output: {o-1 12.5} true
}
