package appsyncevents

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// CurrentEvent returns the invocation event from a context that comes from
// Router.Resolve, as Powertools Python's app.current_event: the identity,
// the request headers, the channel and its namespace, the operation and the
// events. Treat it as read-only: concurrent handlers share it.
func CurrentEvent(ctx context.Context) (*eventsource.AppSyncEventsEvent, bool) {
	inv, ok := ctx.Value(invocationKey{}).(*invocation)
	if !ok {
		return nil, false
	}
	return inv.current(), true
}

// invocationKey keys the invocation in the context.
type invocationKey struct{}

// invocation is an event decoded for routing: the eventsource view with
// the events kept raw, so that each payload decodes once into the handler's
// type.
type invocation struct {
	wire    wire
	events  []rawEvent
	current func() *eventsource.AppSyncEventsEvent
}

// wire decodes an event: its Events shadow those of the embedded event.
type wire struct {
	eventsource.AppSyncEventsEvent

	Events jsontext.Value `json:"events"`
}

// rawEvent is a published event with its payload as it came.
type rawEvent struct {
	ID      string         `json:"id"`
	Payload jsontext.Value `json:"payload"`
}

// decodeOptions make decoding as lenient as Powertools Python, which reads
// an identity it cannot make sense of (the string "None" of the sample
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

func decode(raw jsontext.Value) (*invocation, error) {
	if raw.Kind() != '{' {
		return nil, &InvalidEventError{Err: errNotObject}
	}
	inv := &invocation{}
	if err := json.Unmarshal(raw, &inv.wire, decodeOptions); err != nil {
		return nil, &InvalidEventError{Err: err}
	}
	// A subscribe carries no events; anything but an array counts as none.
	if inv.wire.Events.Kind() == '[' {
		if err := json.Unmarshal(inv.wire.Events, &inv.events); err != nil {
			return nil, &InvalidEventError{Err: err}
		}
	}
	inv.current = sync.OnceValue(inv.view)
	return inv, nil
}

// view builds the eventsource view of the invocation, with the
// payloads decoded as generic JSON values.
func (inv *invocation) view() *eventsource.AppSyncEventsEvent {
	ev := inv.wire.AppSyncEventsEvent
	ev.Events = make([]eventsource.AppSyncEvent, len(inv.events))
	for i := range inv.events {
		ev.Events[i].ID = inv.events[i].ID
		if payload := inv.events[i].Payload; len(payload) > 0 {
			var value any
			if err := json.Unmarshal(payload, &value); err == nil {
				ev.Events[i].Payload = value
			}
		}
	}
	return &ev
}

// decodePayload decodes the payload of ev into P; an event without one
// gives the zero P.
func decodePayload[P any](ev *rawEvent) (P, error) {
	var payload P
	if len(ev.Payload) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(ev.Payload, &payload, decodeOptions); err != nil {
		return payload, &PayloadError{ID: ev.ID, Err: err}
	}
	return payload, nil
}

// forEach calls fn for every index below n, one after the other or up to
// limit at a time, and stops at the first error, which it returns: the
// calls still running see their context cancelled, and no other starts.
func forEach(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	if limit > 1 {
		return forEachConcurrent(ctx, n, limit, fn)
	}
	for i := range n {
		if err := fn(ctx, i); err != nil {
			return err
		}
	}
	return nil
}

func forEachConcurrent(ctx context.Context, n, limit int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		wg   sync.WaitGroup
		stop failure
	)
	slots := make(chan struct{}, limit)
	for i := range n {
		slots <- struct{}{}
		if stop.happened() {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			defer func() {
				if v := recover(); v != nil {
					stop.panicked(v)
					cancel(context.Canceled)
				}
			}()
			if err := fn(ctx, i); err != nil {
				stop.failed(err)
				cancel(err)
			}
		})
	}
	wg.Wait()
	return stop.result()
}

// failure records the first failure of concurrent calls: an error, or a
// panic that result raises again in the caller's goroutine, so that it
// reaches aws-lambda-go as a panic of the handler rather than crashing the
// process.
type failure struct {
	mu    sync.Mutex
	err   error
	panic bool
	value any
}

func (f *failure) happened() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err != nil || f.panic
}

func (f *failure) failed(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = err
	}
}

func (f *failure) panicked(v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.panic {
		f.panic, f.value = true, v
	}
}

func (f *failure) result() error {
	if f.panic {
		panic(f.value)
	}
	return f.err
}
