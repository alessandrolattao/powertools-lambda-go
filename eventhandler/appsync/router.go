package appsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// AnyType registers a resolver for a field whatever its parent type, as
// Powertools Python's default type_name "*" does. A resolver registered for
// the exact type wins over it.
const AnyType = "*"

// Router routes the invocations of AppSync direct Lambda resolvers to the
// resolvers registered for their type and field. Resolve is its Lambda
// handler. The same type serves as the application and as a group of
// resolvers merged into it with Include.
//
// Register every resolver before the first invocation: registration is not
// safe concurrently with Resolve, which only reads, and so serves concurrent
// invocations safely. The zero Router is ready to use.
type Router struct {
	single   map[routeKey]singleRoute
	batch    map[routeKey]batchRoute
	handlers []errorHandler
}

// New returns an empty Router.
func New() *Router {
	return &Router{}
}

// routeKey identifies a resolver; a struct, not "Type.field", so that no
// pair of names can collide.
type routeKey struct {
	typeName, fieldName string
}

type (
	// singleRoute resolves a single invocation into its JSON result.
	singleRoute func(ctx context.Context, it *item) (jsontext.Value, error)
	// batchRoute resolves a batch into the JSON array of its results.
	batchRoute func(ctx context.Context, items []item) (jsontext.Value, error)
	// errorHandler turns err into the response when it handles it.
	errorHandler func(ctx context.Context, err error) (out jsontext.Value, handled bool, herr error)
)

// Resolver registers fn as the resolver of the field fieldName of the type
// typeName (or of any type, with AnyType) for single invocations, the
// default of direct Lambda resolvers. The GraphQL arguments of the field are
// decoded into A (a struct with json tags, a map, or struct{} when the field
// takes none); the result is encoded as JSON. Registering the same function
// for several fields is how Powertools Python's stacked decorators read in
// Go.
//
// The full event is available through CurrentEvent(ctx), the Lambda context
// through lambdacontext.FromContext(ctx). An error from fn fails the
// invocation unless an OnError handler turns it into a response; it is
// returned as it is, so that AppSync shows the name of its type as the
// errorType of the GraphQL error.
//
// Resolver panics if typeName or fieldName is empty or a resolver is
// already registered for them: both are programming errors.
func Resolver[A, R any](r *Router, typeName, fieldName string, fn func(ctx context.Context, args A) (R, error)) {
	r.addSingle(typeName, fieldName, func(ctx context.Context, it *item) (jsontext.Value, error) {
		ev, err := typed[A](it)
		if err != nil {
			return nil, err
		}
		result, err := fn(ctx, ev.Arguments)
		if err != nil {
			return nil, err
		}
		return encode(result, it.event.ParentType(), it.event.Field())
	})
}

// BatchResolver registers fn as the resolver of the field fieldName of the
// type typeName for batch invocations (AppSync's batching of direct Lambda
// resolvers, the answer to the N+1 problem), in aggregate mode: fn receives
// the whole batch and returns one result per event, in the same order.
// This is Powertools Python's batch_resolver with aggregate=True.
//
// A result list of a different length fails the invocation with an
// *InvalidBatchResponseError (Powertools Python only checks that it is a
// list). An error from fn, or from decoding the arguments of any event,
// fails the invocation unless an OnError handler turns it into a response.
//
// It panics as Resolver does.
func BatchResolver[A, R any](r *Router, typeName, fieldName string, fn func(ctx context.Context, events []Event[A]) ([]R, error)) {
	r.addBatch("BatchResolver", typeName, fieldName, func(ctx context.Context, items []item) (jsontext.Value, error) {
		events := make([]Event[A], len(items))
		for i := range items {
			ev, err := typed[A](&items[i])
			if err != nil {
				return nil, err
			}
			events[i] = ev
		}
		results, err := fn(ctx, events)
		if err != nil {
			return nil, err
		}
		first := &items[0].event
		if len(results) != len(items) {
			return nil, &InvalidBatchResponseError{
				TypeName: first.ParentType(), FieldName: first.Field(), Items: len(items), Results: len(results),
			}
		}
		return encode(results, first.ParentType(), first.Field())
	})
}

// OnError registers fn as the handler of the errors of type E: when
// resolving an invocation fails with an error that has an E in its chain
// (errors.AsType), what fn returns, encoded as JSON, becomes the response
// of the invocation instead, as with Powertools Python's exception_handler.
// An error from fn fails the invocation. The context fn receives carries
// the event (CurrentEvent, CurrentBatch) unless the payload itself was
// invalid.
//
// Handlers are tried in the order they were registered and the first match
// wins, so register the most specific types first. They see every failure
// of Resolve: the resolver's own errors, *ResolverNotFoundError,
// *ArgumentsError, *InvalidBatchResponseError and *InvalidEventError, but
// not the item errors a BatchItemResolver turns into null results.
func OnError[E error, R any](r *Router, fn func(ctx context.Context, err E) (R, error)) {
	r.handlers = append(r.handlers, func(ctx context.Context, err error) (jsontext.Value, bool, error) {
		target, ok := errors.AsType[E](err)
		if !ok {
			return nil, false, nil
		}
		result, herr := fn(ctx, target)
		if herr != nil {
			return nil, true, herr
		}
		out, merr := json.Marshal(result)
		if merr != nil {
			return nil, true, fmt.Errorf("appsync: encode the error handler's response: %w", merr)
		}
		return out, true, nil
	})
}

// Include adds the resolvers and error handlers of other to r, as Powertools
// Python's include_router. Resolvers are copied: register them on other
// before including it. Error handlers of other come after those of r.
//
// Include panics if r and other both have a resolver for the same type and
// field (Powertools Python lets other overwrite it silently).
func (r *Router) Include(other *Router) {
	for key, route := range other.single {
		r.putSingle(key, route)
	}
	for key, route := range other.batch {
		r.putBatch(key, route)
	}
	r.handlers = append(r.handlers, other.handlers...)
}

// Resolve is the Lambda handler: it resolves the invocation event with the
// registered resolvers, telling a single invocation (an object) from a batch
// (an array) by the payload, and returns the JSON response for AppSync.
//
//	lambda.Start(app.Resolve)
//
// It composes with middlewares as any handler does. Values a middleware
// stores in ctx reach the resolvers: this is how Powertools Python's
// append_context reads in Go. An empty batch resolves to an empty array.
func (r *Router) Resolve(ctx context.Context, event jsontext.Value) (jsontext.Value, error) {
	inv, err := decodeInvocation(event)
	if err == nil {
		// The error handlers see the event too.
		ctx = context.WithValue(ctx, invocationKey{}, inv)
		var out jsontext.Value
		if out, err = r.dispatch(ctx, inv); err == nil {
			return out, nil
		}
	}
	for _, handle := range r.handlers {
		if out, handled, herr := handle(ctx, err); handled {
			return out, herr
		}
	}
	return nil, err
}

// errNotEvent is the cause of an InvalidEventError for a payload of the
// wrong kind.
var errNotEvent = errors.New("the payload is neither an object nor an array")

// decodeInvocation tells a single invocation (an object) from a batch (an
// array) and decodes it.
func decodeInvocation(event jsontext.Value) (*invocation, error) {
	inv := &invocation{raw: event}
	var err error
	switch event.Kind() {
	case '{':
		inv.items, err = decodeItems(event, false)
	case '[':
		inv.batch = true
		inv.items, err = decodeItems(event, true)
	default:
		err = &InvalidEventError{Err: errNotEvent}
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

func (r *Router) dispatch(ctx context.Context, inv *invocation) (jsontext.Value, error) {
	if !inv.batch {
		it := &inv.items[0]
		typeName, fieldName := it.event.ParentType(), it.event.Field()
		debug.Logger().DebugContext(ctx, "processing direct resolver event", "type", typeName, "field", fieldName)
		route, ok := find(r.single, typeName, fieldName)
		if !ok {
			return nil, &ResolverNotFoundError{TypeName: typeName, FieldName: fieldName}
		}
		return route(ctx, it)
	}

	if len(inv.items) == 0 {
		return jsontext.Value("[]"), nil
	}
	// AppSync batches the requests of one resolver: the first event names
	// the field of all of them.
	typeName, fieldName := inv.items[0].event.ParentType(), inv.items[0].event.Field()
	debug.Logger().DebugContext(ctx, "processing batch resolver event",
		"type", typeName, "field", fieldName, "size", len(inv.items))
	route, ok := find(r.batch, typeName, fieldName)
	if !ok {
		return nil, &ResolverNotFoundError{TypeName: typeName, FieldName: fieldName, Batch: true}
	}
	return route(ctx, inv.items)
}

// find looks a resolver up by its exact type first, then by AnyType.
func find[T any](routes map[routeKey]T, typeName, fieldName string) (T, bool) {
	if route, ok := routes[routeKey{typeName, fieldName}]; ok {
		return route, true
	}
	route, ok := routes[routeKey{AnyType, fieldName}]
	return route, ok
}

func (r *Router) addSingle(typeName, fieldName string, route singleRoute) {
	r.putSingle(checkNames("Resolver", typeName, fieldName), route)
}

func (r *Router) addBatch(kind, typeName, fieldName string, route batchRoute) {
	r.putBatch(checkNames(kind, typeName, fieldName), route)
}

func (r *Router) putSingle(key routeKey, route singleRoute) {
	if r.single == nil {
		r.single = map[routeKey]singleRoute{}
	}
	if _, dup := r.single[key]; dup {
		panic(fmt.Sprintf("appsync: a resolver is already registered for %q", key.typeName+"."+key.fieldName))
	}
	r.single[key] = route
}

func (r *Router) putBatch(key routeKey, route batchRoute) {
	if r.batch == nil {
		r.batch = map[routeKey]batchRoute{}
	}
	if _, dup := r.batch[key]; dup {
		panic(fmt.Sprintf("appsync: a batch resolver is already registered for %q", key.typeName+"."+key.fieldName))
	}
	r.batch[key] = route
}

// checkNames rejects the registrations Powertools Python accepts but can
// never match, such as a missing field name.
func checkNames(kind, typeName, fieldName string) routeKey {
	if typeName == "" || fieldName == "" {
		panic(fmt.Sprintf("appsync: %s needs a type name (or appsync.AnyType) and a field name, got %q and %q",
			kind, typeName, fieldName))
	}
	return routeKey{typeName, fieldName}
}

// encode encodes the result of a resolver.
func encode(result any, typeName, fieldName string) (jsontext.Value, error) {
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("appsync: encode the result of '%s.%s': %w", typeName, fieldName, err)
	}
	return out, nil
}
