package appsyncevents_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/lambda/messages"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/appsyncevents"
)

type Message struct {
	Text string `json:"text"`
}

func ExampleOnPublish() {
	app := appsyncevents.New()
	appsyncevents.OnPublish(app, "/default/channel", func(_ context.Context, msg Message) (Message, error) {
		if msg.Text == "" {
			return Message{}, errors.New("empty message") // this event only is rejected
		}
		return Message{Text: strings.ToUpper(msg.Text)}, nil
	})

	// In main: lambda.Start(app.Resolve)
	event := jsontext.Value(`{
		"info": {"operation": "PUBLISH", "channel": {"path": "/default/channel"}},
		"events": [{"id": "1", "payload": {"text": "hello"}}, {"id": "2", "payload": {"text": ""}}]
	}`)
	out, err := app.Resolve(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"events":[{"id":"1","payload":{"text":"HELLO"}},{"id":"2","error":"errorString - empty message"}]}
}

func ExampleOnPublishAggregate() {
	app := appsyncevents.New()
	appsyncevents.OnPublishAggregate(app, "/default/*", func(_ context.Context, events []appsyncevents.Event[Message]) ([]appsyncevents.Result[Message], error) {
		// One batch write for every event of the request, in real life.
		results := make([]appsyncevents.Result[Message], len(events))
		for i, event := range events {
			results[i] = appsyncevents.Result[Message]{ID: event.ID, Payload: event.Payload}
			if event.Payload.Text == "" {
				results[i] = appsyncevents.Result[Message]{ID: event.ID, Error: "empty message"}
			}
		}
		return results, nil
	})

	event := jsontext.Value(`{
		"info": {"operation": "PUBLISH", "channel": {"path": "/default/chat/room1"}},
		"events": [{"id": "1", "payload": {"text": "hi"}}, {"id": "2", "payload": {}}]
	}`)
	out, err := app.Resolve(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"events":[{"id":"1","payload":{"text":"hi"}},{"id":"2","error":"empty message"}]}
}

func ExampleRouter_OnSubscribe() {
	app := appsyncevents.New()
	app.OnSubscribe("/*", func(ctx context.Context) error {
		event, _ := appsyncevents.CurrentEvent(ctx)
		if event.Info.Channel.Path != "/default/foo" {
			return &appsyncevents.UnauthorizedError{Message: "You can only subscribe to /default/foo"}
		}
		return nil
	})

	allowed := jsontext.Value(`{"info": {"operation": "SUBSCRIBE", "channel": {"path": "/default/foo"}}}`)
	out, err := app.Resolve(context.Background(), allowed)
	fmt.Println(string(out), err)

	// The invocation fails with the errorType AppSync Events expects: the
	// subscription is refused with HTTP 403.
	denied := jsontext.Value(`{"info": {"operation": "SUBSCRIBE", "channel": {"path": "/default/bar"}}}`)
	_, err = app.Resolve(context.Background(), denied)
	if invokeErr, ok := errors.AsType[messages.InvokeResponse_Error](err); ok {
		fmt.Println(invokeErr.Type, "-", invokeErr.Message)
	}
	// Output:
	// null <nil>
	// UnauthorizedException - You can only subscribe to /default/foo
}

func ExampleRouter_Include() {
	chat := appsyncevents.New()
	appsyncevents.OnPublish(chat, "/chat/*", func(_ context.Context, msg Message) (string, error) {
		return "chat: " + msg.Text, nil
	})

	app := appsyncevents.New()
	appsyncevents.OnPublish(app, "/*", func(_ context.Context, msg Message) (string, error) {
		return "other: " + msg.Text, nil
	})
	app.Include(chat)

	event := jsontext.Value(`{
		"info": {"operation": "PUBLISH", "channel": {"path": "/chat/general"}},
		"events": [{"id": "1", "payload": {"text": "hi"}}]
	}`)
	out, err := app.Resolve(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"events":[{"id":"1","payload":"chat: hi"}]}
}
