package appsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// Event is one AppSync resolver event with its GraphQL arguments decoded
// into A. Batch resolvers receive it; CurrentEvent and CurrentBatch return
// it with the arguments as a map.
//
// Every member of eventsource.AppSyncResolverEvent is promoted (Identity,
// Source, Request, Prev, Info, Stash, and the Field and ParentType
// methods). Arguments shadows the untyped map of the embedded event.
type Event[A any] struct {
	eventsource.AppSyncResolverEvent

	// Arguments are the arguments of the field being resolved.
	Arguments A

	// source is the parent object as AppSync sent it, so that DecodeSource
	// does not go through the map and lose large numbers.
	source jsontext.Value
}

// DecodeSource decodes the parent object of the field (the source, null for
// a root field of Query or Mutation) into S. An event built by hand, without
// the raw source, decodes its Source map instead.
func (e Event[A]) DecodeSource[S any]() (S, error) {
	var out S
	raw := e.source
	if raw == nil && e.Source != nil {
		var err error
		if raw, err = json.Marshal(e.Source); err != nil {
			return out, fmt.Errorf("appsync: encode the source: %w", err)
		}
	}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out, decodeOptions); err != nil {
		return out, fmt.Errorf("appsync: decode the source: %w", err)
	}
	return out, nil
}

// CurrentEvent returns the event of a single invocation from a context that
// comes from Router.Resolve, as Powertools Python's app.current_event. A
// batch invocation has none: see CurrentBatch.
func CurrentEvent(ctx context.Context) (*Event[map[string]any], bool) {
	inv, ok := ctx.Value(invocationKey{}).(*invocation)
	if !ok || inv.batch {
		return nil, false
	}
	ev := inv.items[0].untyped()
	return &ev, true
}

// CurrentBatch returns the events of a batch invocation from a context that
// comes from Router.Resolve, as Powertools Python's
// app.current_batch_event. A single invocation has none: see CurrentEvent.
func CurrentBatch(ctx context.Context) ([]Event[map[string]any], bool) {
	inv, ok := ctx.Value(invocationKey{}).(*invocation)
	if !ok || !inv.batch {
		return nil, false
	}
	events := make([]Event[map[string]any], len(inv.items))
	for i := range inv.items {
		events[i] = inv.items[i].untyped()
	}
	return events, true
}

// DecodeEvent decodes the payload of the invocation, as AppSync sent it,
// into T: an object for a single invocation, an array for a batch. It is the
// Go side of Powertools Python's data_model: T is a type of your own that
// embeds eventsource.AppSyncResolverEvent (or a slice of them) and adds the
// members and methods your resolvers need.
func DecodeEvent[T any](ctx context.Context) (T, error) {
	var out T
	inv, ok := ctx.Value(invocationKey{}).(*invocation)
	if !ok {
		return out, ErrNoEvent
	}
	if err := json.Unmarshal(inv.raw, &out, decodeOptions); err != nil {
		return out, fmt.Errorf("appsync: decode the event: %w", err)
	}
	return out, nil
}

// invocationKey keys the invocation in the context.
type invocationKey struct{}

// invocation is what Resolve keeps in the context for the getters.
type invocation struct {
	raw   jsontext.Value
	items []item
	batch bool
}

// item is one resolver event decoded from the payload: the eventsource view
// and the raw members that typed resolvers decode again.
type item struct {
	event     eventsource.AppSyncResolverEvent
	arguments jsontext.Value
	source    jsontext.Value
}

// wireItem decodes an event: its Arguments and Source shadow the maps of
// the embedded event, so that they stay raw.
type wireItem struct {
	eventsource.AppSyncResolverEvent

	Arguments jsontext.Value `json:"arguments"`
	Source    jsontext.Value `json:"source"`
}

// decodeOptions make decoding as lenient as Powertools Python, which reads
// an identity it cannot make sense of (the string "None" of some sample
// events) as no identity rather than failing the invocation.
var decodeOptions = json.WithUnmarshalers(json.UnmarshalFromFunc(
	func(dec *jsontext.Decoder, identity **eventsource.AppSyncIdentity) error {
		if dec.PeekKind() == '{' {
			return errors.ErrUnsupported // the default decoding
		}
		*identity = nil
		if err := dec.SkipValue(); err != nil {
			return fmt.Errorf("skip the identity: %w", err)
		}
		return nil
	}))

// decodeItems decodes a single event, or a batch, into items.
func decodeItems(raw jsontext.Value, batch bool) ([]item, error) {
	var wires []wireItem
	var err error
	if batch {
		err = json.Unmarshal(raw, &wires, decodeOptions)
	} else {
		wires = make([]wireItem, 1)
		err = json.Unmarshal(raw, &wires[0], decodeOptions)
	}
	if err != nil {
		return nil, &InvalidEventError{Err: err}
	}
	items := make([]item, len(wires))
	for i := range wires {
		w := &wires[i]
		items[i] = item{event: w.AppSyncResolverEvent, arguments: w.Arguments, source: w.Source}
		if items[i].event.Arguments, err = objectOrNil(w.Arguments); err != nil {
			return nil, &InvalidEventError{Err: err}
		}
		if items[i].event.Source, err = objectOrNil(w.Source); err != nil {
			return nil, &InvalidEventError{Err: err}
		}
	}
	return items, nil
}

// objectOrNil decodes raw when it is an object, and leaves anything else
// (null, absent, a placeholder string) as no map.
func objectOrNil(raw jsontext.Value) (map[string]any, error) {
	var out map[string]any
	if raw.Kind() == '{' {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode an object: %w", err)
		}
	}
	return out, nil
}

// typed returns the event with its arguments decoded into A.
func typed[A any](it *item) (Event[A], error) {
	ev := Event[A]{AppSyncResolverEvent: it.event, source: it.source}
	if len(it.arguments) == 0 || it.arguments.Kind() == 'n' {
		return ev, nil
	}
	if err := json.Unmarshal(it.arguments, &ev.Arguments, decodeOptions); err != nil {
		return ev, &ArgumentsError{TypeName: it.event.ParentType(), FieldName: it.event.Field(), Err: err}
	}
	return ev, nil
}

// untyped returns the event with its arguments as a map.
func (it *item) untyped() Event[map[string]any] {
	return Event[map[string]any]{AppSyncResolverEvent: it.event, Arguments: it.event.Arguments, source: it.source}
}
