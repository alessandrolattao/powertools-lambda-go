// Package appsync routes the invocations of AWS AppSync GraphQL direct
// Lambda resolvers (and of the Amplify GraphQL Transformer @function
// directive) to typed Go resolvers, one per type and field. It is the Go
// side of Powertools for AWS Lambda (Python)'s AppSyncResolver.
//
// # Getting started
//
// Register a resolver per field with the generic Resolver function. Its
// arguments type receives the GraphQL arguments of the field, decoded from
// JSON; its result is encoded back as JSON. Router.Resolve is the Lambda
// handler:
//
//	type GetTodoArgs struct {
//		ID string `json:"id"`
//	}
//
//	func main() {
//		app := appsync.New()
//		appsync.Resolver(app, "Query", "getTodo", func(ctx context.Context, args GetTodoArgs) (Todo, error) {
//			return todos.Get(ctx, args.ID)
//		})
//		appsync.Resolver(app, "Query", "listTodos", func(ctx context.Context, _ struct{}) ([]Todo, error) {
//			return todos.List(ctx)
//		})
//		lambda.Start(app.Resolve)
//	}
//
// The type comes from info.parentTypeName and the field from info.fieldName
// (the top-level parentTypeName or typeName and fieldName of Amplify
// payloads). A resolver registered for AnyType answers the field under any
// type; one registered for the exact type wins over it. To resolve several
// fields with one function (Powertools Python's stacked decorators), register
// it once per field.
//
// # The event and the Lambda context
//
// Resolve keeps the invocation in the context it passes down. CurrentEvent
// returns the event of a single invocation, CurrentBatch the events of a
// batch, as Event values that promote every member of
// eventsource.AppSyncResolverEvent (Identity, Source, Request, Info, Stash,
// Prev). Event.DecodeSource decodes the parent object of the field into a Go
// type. DecodeEvent decodes the whole payload into a type of your own, the
// Go side of Powertools Python's data_model. The Lambda context comes from
// lambdacontext.FromContext(ctx).
//
// Powertools Python's append_context, the data a handler shares with the
// resolvers of every router, is a value stored in the context before
// Resolve runs, by the handler or by a middleware:
//
//	lambda.Start(func(ctx context.Context, event jsontext.Value) (jsontext.Value, error) {
//		return app.Resolve(context.WithValue(ctx, tenantKey{}, tenantOf(event)), event)
//	})
//
// # Batch resolvers
//
// With batching enabled on the data source, AppSync sends many requests for
// the same field in one invocation, an array of events, which Resolve tells
// from a single event by the payload. BatchResolver registers a resolver
// that receives the whole batch and returns one result per event, in the
// same order: the answer to the N+1 problem. BatchItemResolver registers one
// called once per event instead; a failing event gets a null result, unless
// WithRaiseOnError makes the first failure fail the invocation.
// WithConcurrency resolves several events at the same time. A field may have
// a single resolver and a batch resolver at once: each serves its kind of
// invocation.
//
// # Errors
//
// An error from a resolver fails the invocation; AppSync turns it into a
// GraphQL error whose errorType is the name of the error's type and whose
// message is the error's message. Resolve returns errors as they are, never
// wrapped, so that the type is the resolver's own. To choose the errorType
// freely, return a messages.InvokeResponse_Error from
// github.com/aws/aws-lambda-go/lambda/messages, which aws-lambda-go reports
// as it is.
//
// OnError registers a handler for a type of error: it turns the failure into
// the response of the invocation, as Powertools Python's exception_handler.
// Handlers are tried in registration order and the first whose type is in
// the error's chain wins.
//
// The router's own failures are *ResolverNotFoundError (no resolver for the
// type and field), *ArgumentsError (arguments that do not decode),
// *InvalidBatchResponseError (a batch resolver that returned a different
// number of results) and *InvalidEventError (a payload that is not an
// AppSync event).
//
// # Routers
//
// Resolvers can be registered on several Routers, one per file or domain,
// and merged into the application with Include, Powertools Python's
// include_router.
//
// # Scalars
//
// eventsource.MakeID, AWSDate, AWSTime, AWSDateTime and AWSTimestamp return
// values of the AppSync scalars, as Powertools Python's scalar_types_utils.
//
// # Environment variables
//
// POWERTOOLS_DEBUG writes what the router does to standard error: the
// routing decisions and the events that failed. No other variable applies,
// as in Powertools Python.
//
// # Differences from Python
//
//   - Arguments are a typed struct, not keyword arguments; results are
//     encoded with encoding/json/v2.
//   - There are no async resolvers. BatchItemResolver with WithConcurrency
//     is async_batch_resolver; a single resolver blocks as any Go function
//     does. Python's precedence of a sync batch resolver over an async one
//     for the same field does not arise.
//   - No shared mutable state: current_event, current_batch_event,
//     lambda_context and the context dictionary are context values, so
//     concurrent invocations (Lambda Managed Instances) are safe and nothing
//     needs clearing.
//   - Registering a second resolver for the same type and field, directly or
//     through Include, panics (Python lets the last one win silently), and
//     so does a registration without a field name (Python accepts it and
//     never matches it).
//   - A batch resolver must return exactly one result per event (Python only
//     checks that it returns a list), and an empty batch resolves to an
//     empty array (Python fails with an IndexError).
//   - An identity that is not an object (the string "None" in some sample
//     events) reads as no identity instead of failing the invocation.
//   - Error handlers match by errors.AsType in registration order, not by
//     the most specific class.
package appsync
