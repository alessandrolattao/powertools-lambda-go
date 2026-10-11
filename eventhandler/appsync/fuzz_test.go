package appsync

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// FuzzResolve feeds arbitrary payloads to a router: whatever AppSync (or
// anyone allowed to invoke the function) sends, Resolve answers valid JSON
// or fails with one of its errors, and never panics.
func FuzzResolve(f *testing.F) {
	f.Add([]byte(`{"info":{"parentTypeName":"Query","fieldName":"getTodo"},"arguments":{"id":"1"},"source":{"a":1}}`))
	f.Add([]byte(`{"typeName":"Query","fieldName":"listTodos","identity":"None","prev":"None"}`))
	f.Add([]byte(`[{"info":{"parentTypeName":"Post","fieldName":"related"},"arguments":{"id":"1"}},{"arguments":{"id":2}}]`))
	f.Add([]byte(`[{"fieldName":"each","arguments":null,"source":[1]}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"identity":{"sub":"s","claims":{"a":[1,{"b":null}]}},"request":{"headers":{"h":"v"}}}`))
	f.Add([]byte(`  [ `))

	type args struct {
		ID string `json:"id"`
	}
	app := New()
	Resolver(app, "Query", "getTodo", func(ctx context.Context, a args) (any, error) {
		ev, _ := CurrentEvent(ctx)
		src, err := ev.DecodeSource[map[string]any]()
		return []any{a.ID, src, ev.Identity}, err
	})
	Resolver(app, AnyType, "listTodos", func(ctx context.Context, _ struct{}) (any, error) {
		return DecodeEvent[map[string]any](ctx)
	})
	BatchResolver(app, AnyType, "related", func(_ context.Context, events []Event[args]) ([]string, error) {
		out := make([]string, len(events))
		for i, ev := range events {
			out[i] = ev.Arguments.ID
		}
		return out, nil
	})
	BatchItemResolver(app, AnyType, "each", func(_ context.Context, ev Event[map[string]any]) (any, error) {
		return ev.Arguments, nil
	}, WithConcurrency(2))

	f.Fuzz(func(t *testing.T, payload []byte) {
		out, err := app.Resolve(t.Context(), payload)
		if err != nil {
			if !knownError(err) {
				t.Fatalf("payload %q: unexpected error %T: %v", payload, err, err)
			}
			return
		}
		if !out.IsValid() {
			t.Fatalf("payload %q: invalid JSON response %q", payload, out)
		}
	})
}

func knownError(err error) bool {
	if e, ok := errors.AsType[*InvalidEventError](err); ok {
		return e != nil
	}
	if e, ok := errors.AsType[*ResolverNotFoundError](err); ok {
		return e != nil
	}
	if e, ok := errors.AsType[*ArgumentsError](err); ok {
		return e != nil
	}
	// The resolvers above only fail when the source or the event does not
	// decode.
	return strings.HasPrefix(err.Error(), "appsync: decode the ")
}
