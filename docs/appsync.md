# GraphQL API

The `appsync` package routes the invocations of AWS AppSync GraphQL direct
Lambda resolvers to typed Go resolvers, one per GraphQL type and field.

```go
import "github.com/alessandrolattao/powertools-lambda-go/eventhandler/appsync"
```

## Key features

- Map a function to a GraphQL type and field, or to a field of any type
- GraphQL arguments decoded into a Go struct, results encoded as JSON
- Typed access to the parent object (the source), the identity and the request
- Batch resolvers for AppSync's batching: the whole batch at once (the answer
  to the N+1 problem) or one event at a time, with bounded concurrency
- Error handlers by error type, and errors AppSync shows with a meaningful
  `errorType`
- Routers to split the resolvers of a large API across files

## Terminology

**[Direct Lambda resolver](https://docs.aws.amazon.com/appsync/latest/devguide/direct-lambda-reference.html)**:
an AppSync resolver that invokes a Lambda function without VTL or JavaScript
mapping code; the function's response becomes the value of the field.

**[Batching resolvers](https://docs.aws.amazon.com/appsync/latest/devguide/tutorial-lambda-resolvers.html#advanced-use-case-batching)**:
AppSync groups the requests for one field into a single invocation, an array
of events, instead of invoking the function once per request.

## Getting started

### Required resources

An AppSync GraphQL API whose data source is the Lambda function, and the
permission for AppSync to invoke it. Routing needs nothing else. This is the
schema of the first examples:

```graphql
schema {
    query: Query
    mutation: Mutation
}

type Query {
    getTodo(id: ID!): Todo
    listTodos: [Todo]
}

type Mutation {
    createTodo(title: String!): Todo
}

type Todo {
    id: ID!
    userId: String
    title: String
    completed: Boolean
}
```

### Resolver functions

`appsync.Resolver` registers a function for a type and a field. A type is a
GraphQL type such as `Query`, `Mutation` or `Todo`; a field is one of its
fields, such as `getTodo`. The GraphQL arguments of the field are decoded
into the function's arguments type (a struct with `json` tags, or
`struct{}` for a field without arguments), and what the function returns is
encoded as JSON. `Router.Resolve` is the Lambda handler.

The query `getTodo(id: "todo_id_value")` calls the `getTodo` resolver below
with `GetTodoArgs{ID: "todo_id_value"}`:

```go
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/appsync"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

type Todo struct {
	ID        string `json:"id"`
	UserID    string `json:"userId,omitempty"`
	Title     string `json:"title"`
	Completed bool   `json:"completed"`
}

type GetTodoArgs struct {
	ID string `json:"id"`
}

type CreateTodoArgs struct {
	Title string `json:"title"`
}

func main() {
	app := appsync.New()
	appsync.Resolver(app, "Query", "getTodo", func(ctx context.Context, args GetTodoArgs) (Todo, error) {
		return Todo{ID: args.ID, Title: "Buy milk"}, nil // fetch it from your data store
	})
	appsync.Resolver(app, "Query", "listTodos", func(ctx context.Context, _ struct{}) ([]Todo, error) {
		return []Todo{{ID: "1", Title: "Buy milk"}}, nil
	})
	appsync.Resolver(app, "Mutation", "createTodo", func(ctx context.Context, args CreateTodoArgs) (Todo, error) {
		return Todo{ID: eventsource.MakeID(), Title: args.Title}, nil
	})
	lambda.Start(app.Resolve)
}
```

The type comes from `info.parentTypeName` and the field from
`info.fieldName`; events of the Amplify GraphQL Transformer `@function`
directive, which name them at the top level, are understood too.

`appsync.AnyType` (`"*"`) registers a resolver for a field under any type. A
resolver registered for the exact type wins over it.

### Scalar functions

When working with
[AWS AppSync scalar types](https://docs.aws.amazon.com/appsync/latest/devguide/scalars.html),
the `eventsource` package generates values in the format AppSync expects:

```go
id := eventsource.MakeID()                  // ID
date := eventsource.AWSDate(0)              // AWSDate, UTC; AWSDate(2) for UTC+2
clock := eventsource.AWSTime(0)             // AWSTime
instant := eventsource.AWSDateTime(0)       // AWSDateTime
timestamp := eventsource.AWSTimestamp()     // AWSTimestamp
```

| Scalar type      | Function                      | Sample value                           |
| ---------------- | ----------------------------- | -------------------------------------- |
| **ID**           | `eventsource.MakeID`          | `e916c84d-48b6-484c-bef3-cee3e4d86ebf` |
| **AWSDate**      | `eventsource.AWSDate`         | `2022-07-08Z`                          |
| **AWSTime**      | `eventsource.AWSTime`         | `15:11:00.189Z`                        |
| **AWSDateTime**  | `eventsource.AWSDateTime`     | `2022-07-08T15:11:00.189Z`             |
| **AWSTimestamp** | `eventsource.AWSTimestamp`    | `1657293060`                           |

## Advanced

### Nested mappings

A function resolves as many fields as you register it for:

```go
type LocationArgs struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Location struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

getLocations := func(ctx context.Context, args LocationArgs) ([]Location, error) {
	return []Location{{Name: args.Name, Description: args.Description}}, nil
}
appsync.Resolver(app, appsync.AnyType, "listLocations", getLocations)
appsync.Resolver(app, appsync.AnyType, "locations", getLocations)
```

### Accessing the event and the parent object

`appsync.CurrentEvent` returns the event of the invocation from the context
the resolver receives: the identity of the caller, the request headers, the
`info` of the field, the stash, the result of the previous pipeline function
and the source, the parent object of the field. `DecodeSource` decodes the
source into a Go type. The Lambda context comes from
`lambdacontext.FromContext(ctx)`.

```go
type Merchant struct {
	AccountType string `json:"accountType"`
}

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
```

### Concurrency

Go functions block, so there are no async resolvers. A resolver that needs
several things at once starts goroutines; a batch resolver that handles one
event at a time takes `WithConcurrency`, see
[Concurrent batch resolvers](#concurrent-batch-resolvers).

### Custom data models

Powertools Python lets you subclass its event to add helpers. In Go, decode
the event into a type of your own, embedding
`eventsource.AppSyncResolverEvent`, with `appsync.DecodeEvent`:

```go
type MyEvent struct {
	eventsource.AppSyncResolverEvent
}

func (e MyEvent) CountryViewer() string {
	return e.Request.Headers["cloudfront-viewer-country"]
}

func (e MyEvent) APIKey() string {
	return e.Request.Headers["x-api-key"]
}

appsync.Resolver(app, "Query", "listLocations", func(ctx context.Context, _ struct{}) ([]Location, error) {
	event, err := appsync.DecodeEvent[MyEvent](ctx)
	if err != nil {
		return nil, err
	}
	slog.DebugContext(ctx, "request origin", "country", event.CountryViewer())
	return []Location{{Name: "Perry, James and Carroll"}}, nil
})
```

For a batch invocation, decode into a slice: `appsync.DecodeEvent[[]MyEvent](ctx)`.

### Split operations with Router

As the number of resolvers grows, register them on routers of their own, one
per file or domain, and include them in the application with `Include`:

```go
// locations.go
func LocationsRouter() *appsync.Router {
	router := appsync.New()
	appsync.Resolver(router, appsync.AnyType, "listLocations", listLocations)
	appsync.Resolver(router, appsync.AnyType, "locations", listLocations)
	return router
}

// main.go
func main() {
	app := appsync.New()
	app.Include(LocationsRouter())
	lambda.Start(app.Resolve)
}
```

Registering two resolvers for the same type and field, on one router or
through `Include`, panics when the function starts.

#### Sharing contextual data

Data the handler shares with every resolver (Powertools Python's
`append_context`) goes in the context before `Resolve` runs. Nothing outlives
the invocation, and concurrent invocations never see each other's data.

```go
type adminKey struct{}

appsync.Resolver(router, appsync.AnyType, "listLocations", func(ctx context.Context, args LocationArgs) ([]Location, error) {
	if admin, ok := ctx.Value(adminKey{}).(bool); ok && admin {
		return []Location{{Name: args.Name, Description: args.Description}}, nil
	}
	return []Location{}, nil
})

lambda.Start(func(ctx context.Context, event jsontext.Value) (jsontext.Value, error) {
	return app.Resolve(context.WithValue(ctx, adminKey{}, true), event)
})
```

The same works from a middleware wrapping `app.Resolve`.

### Exception handling

`appsync.OnError` registers a handler for a type of error: when resolving
fails with an error of that type anywhere in its chain, what the handler
returns becomes the response of the invocation. Handlers are tried in the
order they were registered, and the first match wins, so register the most
specific types first.

```go
type ValidationError struct{ Field string }

func (e *ValidationError) Error() string { return "invalid " + e.Field }

appsync.OnError(app, func(ctx context.Context, err *ValidationError) (map[string]string, error) {
	return map[string]string{"message": "error"}, nil
})
appsync.Resolver(app, appsync.AnyType, "createSomething", func(ctx context.Context, _ struct{}) (string, error) {
	return "", &ValidationError{Field: "name"}
})
```

An error no handler takes fails the invocation, and AppSync reports it as a
GraphQL error: its `errorType` is the name of the error's Go type (as
aws-lambda-go reports it), its `message` the error's message. `Resolve`
returns the resolver's error as it is, never wrapped, so that the type stays
the resolver's own. To choose the `errorType` freely, return a
`messages.InvokeResponse_Error` from
`github.com/aws/aws-lambda-go/lambda/messages`:

```go
return Todo{}, messages.InvokeResponse_Error{Type: "NotFound", Message: "todo " + args.ID + " not found"}
```

The router's own failures are `*appsync.ResolverNotFoundError` (no resolver
for the type and field), `*appsync.ArgumentsError` (arguments that do not
decode into the arguments type), `*appsync.InvalidBatchResponseError` (a
batch resolver that returned a different number of results) and
`*appsync.InvalidEventError` (a payload that is not an AppSync event).

### Batch processing

#### Understanding the N+1 problem

When AWS AppSync has
[batching enabled for Lambda resolvers](https://docs.aws.amazon.com/appsync/latest/devguide/tutorial-lambda-resolvers.html#advanced-use-case-batching),
it groups as many requests as possible into one invocation.

Say a query `listPosts` returns posts, and for each post the client asks for
`relatedPosts`. Without batching, AppSync invokes the function once for the
posts and then once per post for its related posts: N+1 invocations. With
batching, the N requests for `relatedPosts` arrive in one invocation, an
array of events, and `Resolve` routes it to the batch resolver of the field.

#### Batch resolvers

`appsync.BatchResolver` hands the whole batch to the function, which returns
one result per event, in the same order, so that AppSync can match results
and requests:

```go
type Post struct {
	PostID string `json:"post_id"`
}

appsync.BatchResolver(app, "Post", "relatedPosts", func(ctx context.Context, events []appsync.Event[struct{}]) ([]*Post, error) {
	ids := make([]string, len(events))
	for i, event := range events {
		post, err := event.DecodeSource[Post]() // the post whose related posts are requested
		if err != nil {
			return nil, err
		}
		ids[i] = post.PostID
	}
	related := fetchRelated(ctx, ids) // one query for the whole batch
	results := make([]*Post, len(events))
	for i, id := range ids {
		results[i] = related[id] // nil (null) when there is none
	}
	return results, nil
})
```

Each `appsync.Event` carries the arguments of its request, decoded into the
type parameter, and every member of `eventsource.AppSyncResolverEvent`
(identity, source, info, request). A result list of a different length
fails the invocation with `*appsync.InvalidBatchResponseError`. A field can
have a batch resolver and a single resolver at the same time: each serves
its kind of invocation.

#### Processing items individually

`appsync.BatchItemResolver` calls the function once per event and returns
the results in the order of the batch. It moves the N+1 problem from AppSync
into the function rather than solving it.

```go
type RelatedArgs struct {
	PostID string `json:"post_id"`
}

appsync.BatchItemResolver(app, "Query", "relatedPosts", func(ctx context.Context, event appsync.Event[RelatedArgs]) (*Post, error) {
	return fetchPost(ctx, event.Arguments.PostID)
})
```

A failing event gets a `null` result, and the other events are resolved.

#### Raise on error

`WithRaiseOnError(true)` makes the first failure fail the whole invocation
instead, through the error handlers:

```go
appsync.BatchItemResolver(app, "Query", "relatedPosts", resolvePost, appsync.WithRaiseOnError(true))
```

#### Concurrent batch resolvers

`WithConcurrency(n)` resolves up to `n` events of the batch at the same time,
each in its own goroutine, as Powertools Python's `async_batch_resolver`
gathers them. The results keep the order of the batch. With
`WithRaiseOnError`, the first failure cancels the context of the events still
running and no further event starts. A panic in a resolver is raised again in
the invocation's goroutine, where aws-lambda-go reports it.

```go
appsync.BatchItemResolver(app, "Query", "relatedPosts", resolvePost, appsync.WithConcurrency(10))
```

## Testing your code

There are no external calls: build an event (or load a sample one) and call
`Resolve`.

```go
func TestGetTodo(t *testing.T) {
	app := newApp()
	event := jsontext.Value(`{
		"arguments": {"id": "todo-1"},
		"info": {"parentTypeName": "Query", "fieldName": "getTodo"}
	}`)
	out, err := app.Resolve(t.Context(), event)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"todo-1","title":"Buy milk","completed":false}`, string(out))
}
```

A batch resolver is a plain function too: call it with hand-built events.
`DecodeSource` falls back to the `Source` map when the event was not decoded
from a payload.

```go
events := []appsync.Event[struct{}]{{AppSyncResolverEvent: eventsource.AppSyncResolverEvent{
	Source: map[string]any{"post_id": "1"},
}}}
```

To exercise the Lambda context, attach one with
`lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{...})`.

## Differences from Powertools Python

- Arguments are a typed struct, not keyword arguments; results are encoded
  with `encoding/json/v2`.
- No async resolvers: `BatchItemResolver` with `WithConcurrency` is
  `async_batch_resolver` (aggregate or not, one batch resolver per field, so
  the precedence of a sync batch resolver over an async one does not arise).
- `current_event`, `current_batch_event`, `lambda_context` and the context
  dictionary are context values, safe under concurrent invocations (Lambda
  Managed Instances), with nothing to clear.
- `data_model` is `DecodeEvent` into a type of your own.
- Registering a second resolver for the same type and field, directly or
  through `Include`, panics (Python lets the last one win); so does a
  registration without a field name, which Python accepts and never matches.
- A batch resolver must return exactly one result per event (Python only
  checks that it returns a list); an empty batch resolves to an empty array
  (Python fails).
- Error handlers match with `errors.AsType`, first registered first, where
  Python looks up the exception's class hierarchy.
- An identity that is not an object (the string `"None"` of some sample
  events) reads as no identity.
