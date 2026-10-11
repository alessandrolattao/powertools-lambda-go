package appsync_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/appsync"
)

type Todo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Completed bool   `json:"completed"`
}

type GetTodoArgs struct {
	ID string `json:"id"`
}

func ExampleResolver() {
	app := appsync.New()
	appsync.Resolver(app, "Query", "getTodo", func(_ context.Context, args GetTodoArgs) (Todo, error) {
		return Todo{ID: args.ID, Title: "Buy milk"}, nil
	})

	// In main: lambda.Start(app.Resolve)
	event := jsontext.Value(`{
		"arguments": {"id": "todo-1"},
		"info": {"parentTypeName": "Query", "fieldName": "getTodo"}
	}`)
	out, err := app.Resolve(context.Background(), event)
	fmt.Println(string(out), err)
	// Output: {"id":"todo-1","title":"Buy milk","completed":false} <nil>
}

func ExampleCurrentEvent() {
	type Merchant struct {
		AccountType string `json:"accountType"`
	}
	app := appsync.New()
	// A field of a non-root type: its parent object is the source.
	appsync.Resolver(app, "Merchant", "extraInfo", func(ctx context.Context, _ struct{}) (map[string]string, error) {
		event, _ := appsync.CurrentEvent(ctx)
		merchant, err := event.DecodeSource[Merchant]()
		if err != nil {
			return nil, err
		}
		method := "USD"
		if merchant.AccountType == "NEW" {
			method = "BTC"
		}
		return map[string]string{"preferredPaymentMethod": method}, nil
	})

	event := jsontext.Value(`{
		"source": {"accountType": "NEW"},
		"info": {"parentTypeName": "Merchant", "fieldName": "extraInfo"}
	}`)
	out, err := app.Resolve(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"preferredPaymentMethod":"BTC"}
}

func ExampleBatchResolver() {
	type Post struct {
		PostID string `json:"post_id"`
	}
	titles := map[string]string{"1": "post1", "2": "post2"}

	app := appsync.New()
	// One invocation for the relatedPosts field of every post of the query.
	appsync.BatchResolver(app, "Post", "relatedPosts", func(_ context.Context, events []appsync.Event[struct{}]) ([]string, error) {
		results := make([]string, len(events))
		for i, event := range events {
			post, err := event.DecodeSource[Post]()
			if err != nil {
				return nil, err
			}
			results[i] = titles[post.PostID] // one batch query in real life
		}
		return results, nil
	})

	event := jsontext.Value(`[
		{"source": {"post_id": "1"}, "info": {"parentTypeName": "Post", "fieldName": "relatedPosts"}},
		{"source": {"post_id": "2"}, "info": {"parentTypeName": "Post", "fieldName": "relatedPosts"}}
	]`)
	out, err := app.Resolve(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: ["post1","post2"]
}

func ExampleBatchItemResolver() {
	type Args struct {
		PostID string `json:"post_id"`
	}
	titles := map[string]string{"1": "post1"}

	app := appsync.New()
	appsync.BatchItemResolver(app, "Query", "relatedPosts", func(_ context.Context, event appsync.Event[Args]) (string, error) {
		title, ok := titles[event.Arguments.PostID]
		if !ok {
			return "", errors.New("post not found") // a null result, unless WithRaiseOnError(true)
		}
		return title, nil
	}, appsync.WithConcurrency(10))

	event := jsontext.Value(`[
		{"arguments": {"post_id": "1"}, "info": {"parentTypeName": "Query", "fieldName": "relatedPosts"}},
		{"arguments": {"post_id": "9"}, "info": {"parentTypeName": "Query", "fieldName": "relatedPosts"}}
	]`)
	out, err := app.Resolve(context.Background(), event)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: ["post1",null]
}

// ValidationError is an error of the application's own.
type ValidationError struct{ Field string }

func (e *ValidationError) Error() string { return "invalid " + e.Field }

func ExampleOnError() {
	app := appsync.New()
	appsync.OnError(app, func(_ context.Context, err *ValidationError) (map[string]string, error) {
		return map[string]string{"error": err.Error()}, nil
	})
	appsync.Resolver(app, appsync.AnyType, "createSomething", func(context.Context, struct{}) (string, error) {
		return "", &ValidationError{Field: "name"}
	})

	out, err := app.Resolve(context.Background(), jsontext.Value(`{"fieldName": "createSomething"}`))
	fmt.Println(string(out), err)
	// Output: {"error":"invalid name"} <nil>
}

func ExampleRouter_Include() {
	// In a file of its own, the resolvers of one domain.
	locations := appsync.New()
	appsync.Resolver(locations, appsync.AnyType, "listLocations", func(context.Context, struct{}) ([]string, error) {
		return []string{"Perry, James and Carroll"}, nil
	})

	app := appsync.New()
	app.Include(locations)

	out, err := app.Resolve(context.Background(), jsontext.Value(`{"typeName": "Query", "fieldName": "listLocations"}`))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: ["Perry, James and Carroll"]
}

func ExampleRouter_Resolve_notFound() {
	app := appsync.New()
	_, err := app.Resolve(context.Background(), jsontext.Value(`{"typeName": "Query", "fieldName": "getTodo"}`))

	// aws-lambda-go reports the type of the error as the errorType that
	// AppSync shows in the GraphQL error.
	if notFound, ok := errors.AsType[*appsync.ResolverNotFoundError](err); ok {
		fmt.Println(notFound.TypeName, notFound.FieldName)
	}
	fmt.Println(err)
	// Output:
	// Query getTodo
	// No resolver found for 'Query.getTodo'
}
