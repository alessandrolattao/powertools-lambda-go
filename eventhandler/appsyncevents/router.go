package appsyncevents

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// DefaultPath is the channel path Powertools Python registers handlers for
// when none is given: every channel of the default namespace.
const DefaultPath = "/default/*"

// Router routes the invocations of an AppSync Events channel namespace to
// the publish and subscribe handlers registered for their channel path.
// Resolve is its Lambda handler. The same type serves as the application
// and as a group of handlers merged into it with Include.
//
// Register every handler before the first invocation: registration is not
// safe concurrently with Resolve, which serves concurrent invocations
// safely. The zero Router is ready to use.
type Router struct {
	publish   map[string]route[publishFunc]
	subscribe map[string]route[subscribeFunc]
	logger    *slog.Logger

	mu     sync.Mutex
	warned map[string]bool
}

// Option configures a Router.
type Option func(*Router)

// WithLogger sends the router's warnings (a channel without a handler) to
// logger instead of standard error, where Powertools Python's warnings go.
func WithLogger(logger *slog.Logger) Option {
	return func(r *Router) { r.logger = logger }
}

// New returns an empty Router.
func New(opts ...Option) *Router {
	r := &Router{}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

type (
	route[F any] struct {
		pattern pattern
		fn      F
	}
	// publishFunc answers a publish invocation with the whole response.
	publishFunc func(ctx context.Context, inv *invocation) (jsontext.Value, error)
	// subscribeFunc is a subscribe handler.
	subscribeFunc func(ctx context.Context) error
)

// Event is one published event: its identifier and its payload decoded into
// P.
type Event[P any] struct {
	ID      string `json:"id"`
	Payload P      `json:"payload"`
}

// Result is what an aggregate publish handler returns for one event: the
// payload broadcast to the subscribers, or an error that drops the event.
// AppSync matches results and events by ID, not by position.
type Result[R any] struct {
	ID      string
	Payload R
	// Error, when not empty, drops the event and is reported to the
	// publisher in its place.
	Error string
}

// MarshalJSONTo writes {"id","payload"}, or {"id","error"} for a failed
// event, the two shapes AppSync Events accepts.
func (r Result[R]) MarshalJSONTo(enc *jsontext.Encoder) error {
	var err error
	if r.Error != "" {
		err = json.MarshalEncode(enc, failedResult{ID: r.ID, Error: r.Error})
	} else {
		err = json.MarshalEncode(enc, struct {
			ID      string `json:"id"`
			Payload R      `json:"payload"`
		}{r.ID, r.Payload})
	}
	if err != nil {
		return fmt.Errorf("appsyncevents: encode the result of event %s: %w", r.ID, err)
	}
	return nil
}

type failedResult struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// itemResult is the result of one event in per-event mode.
type itemResult struct {
	ID      string         `json:"id"`
	Payload jsontext.Value `json:"payload,omitzero"`
	Error   string         `json:"error,omitempty"`
}

// PublishOption configures a per-event publish handler.
type PublishOption func(*publishConfig)

type publishConfig struct {
	concurrency int
}

// WithConcurrency handles up to n events of the invocation at the same
// time, each in its own goroutine; the default, 1, handles them one after
// the other. It is Powertools Python's async_on_publish, which gathers the
// events concurrently, with a bound. The results keep the order of the
// events either way.
func WithConcurrency(n int) PublishOption {
	return func(c *publishConfig) { c.concurrency = max(n, 1) }
}

// OnPublish registers fn as the publish handler of the channels that match
// path, called once per published event with its payload decoded into P:
// what fn returns, encoded as JSON, is the payload broadcast to the
// subscribers in place of the published one. This is Powertools Python's
// on_publish (and async_on_publish) with aggregate=False.
//
// An error from fn, or a payload that does not decode into P, fails that
// event only: it is reported to AppSync as "<type of the error> - <message>"
// and not broadcast, and the other events go on. An *UnauthorizedError
// instead fails the whole invocation, so that nothing is broadcast
// (Powertools Python reports it as an event error, which its documentation
// says it does not).
//
// Path is an exact channel path ("/default/channel"), a namespace wildcard
// ("/default/*") or "/*"; the most specific match wins. OnPublish panics on
// an invalid path, or on a path that already has a publish handler: both
// are programming errors.
func OnPublish[P, R any](r *Router, path string, fn func(ctx context.Context, payload P) (R, error), opts ...PublishOption) {
	cfg := publishConfig{concurrency: 1}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	r.addPublish(path, func(ctx context.Context, inv *invocation) (jsontext.Value, error) {
		results := make([]itemResult, len(inv.events))
		err := forEach(ctx, len(inv.events), cfg.concurrency, func(ctx context.Context, i int) error {
			ev := &inv.events[i]
			results[i].ID = ev.ID
			out, err := publishOne(ctx, ev, fn)
			if err == nil {
				results[i].Payload = out
				return nil
			}
			if unauthorized, ok := errors.AsType[*UnauthorizedError](err); ok {
				return unauthorized
			}
			debug.Logger().DebugContext(ctx, "failed to process an event", "index", i, "id", ev.ID, "err", err)
			results[i].Error = describe(err)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return encode(publishResponse[itemResult]{Events: results})
	})
}

func publishOne[P, R any](ctx context.Context, ev *rawEvent, fn func(ctx context.Context, payload P) (R, error)) (jsontext.Value, error) {
	payload, err := decodePayload[P](ev)
	if err != nil {
		return nil, err
	}
	result, err := fn(ctx, payload)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("appsyncevents: encode the payload of event %s: %w", ev.ID, err)
	}
	return out, nil
}

// OnPublishAggregate registers fn as the publish handler of the channels
// that match path, called once with all the events of the invocation, their
// payloads decoded into P: Powertools Python's on_publish with
// aggregate=True. fn returns a Result per event to broadcast, or to drop
// with an error; events left out are not broadcast.
//
// An error from fn, or a payload that does not decode into P, fails the
// whole request: AppSync receives {"error": "<type of the error> -
// <message>"}. An *UnauthorizedError fails the invocation instead.
//
// It panics as OnPublish does.
func OnPublishAggregate[P, R any](r *Router, path string, fn func(ctx context.Context, events []Event[P]) ([]Result[R], error)) {
	r.addPublish(path, func(ctx context.Context, inv *invocation) (jsontext.Value, error) {
		events := make([]Event[P], len(inv.events))
		for i := range inv.events {
			payload, err := decodePayload[P](&inv.events[i])
			if err != nil {
				return encode(errorResponse{Error: describe(err)})
			}
			events[i] = Event[P]{ID: inv.events[i].ID, Payload: payload}
		}
		results, err := fn(ctx, events)
		if err != nil {
			if unauthorized, ok := errors.AsType[*UnauthorizedError](err); ok {
				return nil, unauthorized
			}
			return encode(errorResponse{Error: describe(err)})
		}
		if results == nil {
			results = []Result[R]{}
		}
		return encode(publishResponse[Result[R]]{Events: results})
	})
}

// OnSubscribe registers fn as the subscribe handler of the channels that
// match path, Powertools Python's on_subscribe. Returning nil accepts the
// subscription; an *UnauthorizedError rejects it with HTTP 403; any other
// error is reported to AppSync as {"error": "<type of the error> -
// <message>"}. The channel is in CurrentEvent(ctx).Info.Channel.
//
// It panics as OnPublish does.
func (r *Router) OnSubscribe(path string, fn func(ctx context.Context) error) {
	r.putSubscribe(route[subscribeFunc]{pattern: mustParse(path), fn: fn})
}

// Include adds the handlers of other to r, as Powertools Python's
// include_router. Handlers are copied: register them on other before
// including it.
//
// Include panics if r and other both have a publish, or both a subscribe,
// handler for the same path (Powertools Python lets other overwrite it
// silently).
func (r *Router) Include(other *Router) {
	for _, rt := range other.publish {
		r.putPublish(rt)
	}
	for _, rt := range other.subscribe {
		r.putSubscribe(rt)
	}
}

// Resolve is the Lambda handler: it routes the invocation event to the
// handler of its operation (PUBLISH, or anything else for a subscribe) and
// channel path, and returns the response for AppSync Events.
//
//	lambda.Start(app.Resolve)
//
// A publish answers {"events": [...]} with a payload or an error per
// event, or {"error": "..."} when the whole request fails. A subscribe
// answers null, or {"error": "..."}. A publish to a channel without a
// handler warns and returns the events as they came; a subscribe without
// one warns and accepts the subscription.
//
// An *UnauthorizedError fails the invocation with a
// messages.InvokeResponse_Error whose Type is "UnauthorizedException",
// which aws-lambda-go reports as the errorType AppSync Events expects.
// Return it unwrapped from any middleware around Resolve.
func (r *Router) Resolve(ctx context.Context, event jsontext.Value) (jsontext.Value, error) {
	inv, err := decode(event)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, invocationKey{}, inv)
	var out jsontext.Value
	if inv.wire.Info.Operation == eventsource.AppSyncEventsPublish {
		out, err = r.publishEvents(ctx, inv)
	} else {
		out, err = r.subscribeEvents(ctx, inv)
	}
	if unauthorized, ok := errors.AsType[*UnauthorizedError](err); ok {
		return nil, unauthorized.invokeError()
	}
	return out, err
}

func (r *Router) publishEvents(ctx context.Context, inv *invocation) (jsontext.Value, error) {
	channel := inv.wire.Info.Channel.Path
	debug.Logger().DebugContext(ctx, "processing publish events", "path", channel, "events", len(inv.events))
	rt, ok := best(r.publish, channel)
	if !ok {
		if r.firstWarning("publish " + channel) {
			r.warner().WarnContext(ctx, "No resolvers were found for publish operations with this path. "+
				"We will return the entire payload as is", "path", channel)
		}
		events := inv.wire.Events
		if events.Kind() != '[' {
			events = jsontext.Value("[]")
		}
		return encode(rawResponse{Events: events})
	}
	debug.Logger().DebugContext(ctx, "found a publish handler", "path", channel, "pattern", rt.pattern.path)
	return rt.fn(ctx, inv)
}

func (r *Router) subscribeEvents(ctx context.Context, inv *invocation) (jsontext.Value, error) {
	channel := inv.wire.Info.Channel.Path
	debug.Logger().DebugContext(ctx, "processing subscribe event", "path", channel)
	rt, ok := best(r.subscribe, channel)
	if !ok {
		if r.firstWarning("subscribe " + channel) {
			r.warner().WarnContext(ctx, "No resolvers were found for subscribe operations with this path", "path", channel)
		}
		return jsontext.Value("null"), nil
	}
	if err := rt.fn(ctx); err != nil {
		if unauthorized, ok := errors.AsType[*UnauthorizedError](err); ok {
			return nil, unauthorized
		}
		return encode(errorResponse{Error: describe(err)})
	}
	return jsontext.Value("null"), nil
}

// publishResponse is {"events": [...]}.
type publishResponse[T any] struct {
	Events []T `json:"events"`
}

// rawResponse carries the events as AppSync sent them.
type rawResponse struct {
	Events jsontext.Value `json:"events"`
}

// errorResponse is {"error": "..."}, a failed request.
type errorResponse struct {
	Error string `json:"error"`
}

func encode(response any) (jsontext.Value, error) {
	out, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("appsyncevents: encode the response: %w", err)
	}
	return out, nil
}

// maxWarnings bounds the channel paths the router remembers having warned
// about: channel names come from the clients.
const maxWarnings = 1024

// firstWarning reports whether the warning of key is new, so that it is
// written once per cause, as Python's warnings are.
func (r *Router) firstWarning(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned[key] {
		return false
	}
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	if len(r.warned) < maxWarnings {
		r.warned[key] = true
	}
	return true
}

// stderr is where the warnings Powertools Python raises as warnings go.
var stderr = sync.OnceValue(func() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
})

func (r *Router) warner() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return stderr()
}

func mustParse(path string) pattern {
	p, err := parsePattern(path)
	if err != nil {
		panic(err.Error())
	}
	return p
}

func (r *Router) addPublish(path string, fn publishFunc) {
	r.putPublish(route[publishFunc]{pattern: mustParse(path), fn: fn})
}

func (r *Router) putPublish(rt route[publishFunc]) {
	if r.publish == nil {
		r.publish = map[string]route[publishFunc]{}
	}
	if _, dup := r.publish[rt.pattern.path]; dup {
		panic(fmt.Sprintf("appsyncevents: a publish handler is already registered for %q", rt.pattern.path))
	}
	r.publish[rt.pattern.path] = rt
}

func (r *Router) putSubscribe(rt route[subscribeFunc]) {
	if r.subscribe == nil {
		r.subscribe = map[string]route[subscribeFunc]{}
	}
	if _, dup := r.subscribe[rt.pattern.path]; dup {
		panic(fmt.Sprintf("appsyncevents: a subscribe handler is already registered for %q", rt.pattern.path))
	}
	r.subscribe[rt.pattern.path] = rt
}
