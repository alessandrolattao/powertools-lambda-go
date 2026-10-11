// Package appsyncevents handles the publish and subscribe operations of
// AWS AppSync Events channel namespaces whose handler is a Lambda function,
// routing each to the Go handler registered for its channel path. It is the
// Go side of Powertools for AWS Lambda (Python)'s AppSyncEventsResolver.
//
// # Getting started
//
// Register handlers for channel paths and hand Router.Resolve to
// lambda.Start:
//
//	type Message struct {
//		Text string `json:"text"`
//	}
//
//	func main() {
//		app := appsyncevents.New()
//		appsyncevents.OnPublish(app, "/default/channel", func(ctx context.Context, msg Message) (Message, error) {
//			msg.Text = strings.TrimSpace(msg.Text)
//			return msg, nil
//		})
//		app.OnSubscribe("/*", func(ctx context.Context) error {
//			event, _ := appsyncevents.CurrentEvent(ctx)
//			if !allowed(event.Identity, event.Info.Channel.Path) {
//				return &appsyncevents.UnauthorizedError{Message: "not allowed"}
//			}
//			return nil
//		})
//		lambda.Start(app.Resolve)
//	}
//
// OnPublish handles each published event on its own: its payload is decoded
// into the handler's type, and the result, encoded as JSON, is what the
// subscribers receive. OnPublishAggregate hands the handler all the events
// of the publish request at once, for one batch query instead of one per
// event. OnSubscribe authorizes subscriptions.
//
// # Channel paths and precedence
//
// A handler is registered for an exact channel path ("/default/channel"), a
// wildcard covering a namespace or a subtree ("/default/*", "/default/v1/*")
// or every channel ("/*"). When several match, the most specific wins: an
// exact path over any wildcard, a longer wildcard over a shorter one. A
// wildcard is allowed only as the last segment.
//
// A publish without a matching handler warns (once per channel) and returns
// the events as they came, so they are broadcast unchanged; a subscribe
// without one warns and is accepted.
//
// # Errors
//
// With OnPublish, an error fails its event only: AppSync receives
// {"id": ..., "error": "<type> - <message>"} for it, where the type is the
// name of the error's Go type, and does not broadcast it. With
// OnPublishAggregate, the handler sets Result.Error for the events it
// rejects, and an error it returns fails the whole request with
// {"error": "<type> - <message>"}. A subscribe handler's error rejects the
// subscription the same way.
//
// An *UnauthorizedError, from any handler, fails the invocation itself with
// the errorType "UnauthorizedException": AppSync Events refuses the
// subscription with HTTP 403, or broadcasts none of the published events.
// Resolve returns it as a messages.InvokeResponse_Error, which aws-lambda-go
// reports as it is; a middleware around Resolve must return it unwrapped.
//
// # The event and the Lambda context
//
// CurrentEvent returns the invocation event (identity, request headers,
// channel, namespace, operation, events) from the context the handlers
// receive; lambdacontext.FromContext(ctx) the Lambda context. Data shared
// with every handler, Powertools Python's append_context, is a value stored
// in the context before Resolve runs.
//
// # Concurrency
//
// WithConcurrency makes OnPublish handle several events at the same time,
// as Powertools Python's async_on_publish gathers them; the response keeps
// the order of the events. Aggregate and subscribe handlers run once per
// invocation and need no option.
//
// # Routers
//
// Handlers can be registered on several Routers and merged into the
// application with Include, Powertools Python's include_router.
//
// # Environment variables
//
// POWERTOOLS_DEBUG writes what the router does to standard error: the
// routing decisions and the events that failed. No other variable applies,
// as in Powertools Python.
//
// # Differences from Python
//
//   - Payloads are decoded into the handler's type and results encoded with
//     encoding/json/v2. An aggregate handler returns typed Results, so the
//     non-list response Python warns about cannot happen.
//   - An *UnauthorizedError from a per-event publish handler fails the
//     invocation, as the documentation of Powertools Python says; its code
//     reports it as an error of that event instead.
//   - A wildcard matches at a segment boundary: "/default/*" does not match
//     "/defaults/a", which Python's regular expression does; and an exact
//     path wins over a wildcard that ends at the same place ("/a/b" over
//     "/a/b/*" for the channel "/a/b"), where Python's score prefers the
//     wildcard.
//   - There are no async handlers, and so no precedence of a sync handler
//     over an async one: one handler per path, with WithConcurrency for
//     concurrent events.
//   - An invalid path panics at registration (Python warns and skips the
//     handler), and so does a second handler for the same path and
//     operation, directly or through Include (Python lets the last one
//     win).
//   - A publish without events gives an aggregate handler no events
//     (Python gives it one empty placeholder event).
//   - No shared mutable state: current_event, lambda_context and the
//     context dictionary are context values, safe under concurrent
//     invocations (Lambda Managed Instances), with nothing to clear.
package appsyncevents
