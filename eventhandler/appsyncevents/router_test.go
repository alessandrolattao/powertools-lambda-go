package appsyncevents

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/lambda/messages"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// load reads a sample event from testdata (Powertools Python's
// tests/events/appSyncEventsEvent.json and the events of its documentation).
func load(t *testing.T, name string) jsontext.Value {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

// event returns the upstream sample event with the operation, the channel
// path and the events replaced, as the upstream tests do.
func event(t *testing.T, operation, path, events string) jsontext.Value {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(load(t, "appSyncEventsEvent.json"), &doc))
	info, ok := doc["info"].(map[string]any)
	require.True(t, ok)
	info["operation"] = operation
	channel, ok := info["channel"].(map[string]any)
	require.True(t, ok)
	channel["path"] = path
	if events == "" {
		delete(doc, "events")
	} else {
		doc["events"] = jsontext.Value(events)
	}
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	return out
}

func publish(t *testing.T, path, events string) jsontext.Value {
	t.Helper()
	return event(t, "PUBLISH", path, events)
}

func subscribe(t *testing.T, path string) jsontext.Value {
	t.Helper()
	return event(t, "SUBSCRIBE", path, "")
}

// resolveJSON runs app on ev and returns the response.
func resolveJSON(t *testing.T, app *Router, ev jsontext.Value) string {
	t.Helper()
	out, err := app.Resolve(t.Context(), ev)
	require.NoError(t, err)
	return string(out)
}

type data struct {
	Data string `json:"data"`
}

type processed struct {
	Processed bool   `json:"processed"`
	Data      string `json:"data"`
}

// ValueError stands for the Python exception of the upstream tests.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// NotAuthorizedError is the custom exception of the upstream tests.
type NotAuthorizedError struct{ Msg string }

func (e NotAuthorizedError) Error() string { return e.Msg }

const oneEvent = `[{"id":"123","payload":{"data":"test data"}}]`

func TestPublish(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprint("concurrency ", concurrency), func(t *testing.T) {
			t.Parallel()
			app := New()
			OnPublish(app, DefaultPath, func(_ context.Context, payload data) (processed, error) {
				return processed{Processed: true, Data: payload.Data}, nil
			}, WithConcurrency(concurrency))
			got := resolveJSON(t, app, publish(t, "/default/channel", oneEvent))
			assert.JSONEq(t, `{"events":[{"id":"123","payload":{"processed":true,"data":"test data"}}]}`, got)
		})
	}
}

func TestPublishSampleEvents(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, "/default/channel", func(_ context.Context, payload map[string]any) (map[string]any, error) {
		return map[string]any{"original_payload": payload}, nil
	})
	got := resolveJSON(t, app, load(t, "appSyncEventsEvent.json"))
	assert.JSONEq(t, `{"events":[
		{"id":"1","payload":{"original_payload":{"event_1":"data_1"}}},
		{"id":"2","payload":{"original_payload":{"event_2":"data_2"}}}]}`, got)

	app = New()
	OnPublish(app, DefaultPath, func(_ context.Context, payload data) (processed, error) {
		return processed{Processed: true, Data: payload.Data}, nil
	})
	got = resolveJSON(t, app, load(t, "getting_started_with_testing_publish_event.json"))
	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"processed":true,"data":"test data"}}]}`, got)
}

func TestPublishErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"pointer type", &ValueError{Msg: "Test error"}, "ValueError - Test error"},
		{"value type", NotAuthorizedError{Msg: "Not authorized"}, "NotAuthorizedError - Not authorized"},
		{
			"invoke error",
			messages.InvokeResponse_Error{Type: "CustomSecurityException", Message: "Security check failed"},
			"CustomSecurityException - Security check failed",
		},
		{"standard library", errors.New("plain"), "errorString - plain"},
		{"wrapped", fmt.Errorf("context: %w", &ValueError{Msg: "x"}), "wrapError - context: x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, concurrency := range []int{1, 2} {
				app := New()
				OnPublish(app, DefaultPath, func(context.Context, data) (data, error) {
					return data{}, tt.err
				}, WithConcurrency(concurrency))
				got := resolveJSON(t, app, publish(t, "/default/test", oneEvent))
				assert.JSONEq(t, `{"events":[{"id":"123","error":`+string(mustJSON(t, tt.want))+`}]}`, got)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return out
}

func TestPublishMixedSuccessAndErrors(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, DefaultPath, func(_ context.Context, payload data) (map[string]any, error) {
		if payload.Data == "bad data" {
			return nil, &ValueError{Msg: "Bad data detected"}
		}
		return map[string]any{"success": true, "data": payload.Data}, nil
	})
	got := resolveJSON(t, app, publish(t, "/default/channel", `[
		{"id":"123","payload":{"data":"good data"}},
		{"id":"456","payload":{"data":"bad data"}},
		{"id":"789","payload":{"data":"good data again"}},
		{"id":"000","payload":{"data":42}}]`))
	var response struct {
		Events []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &response))
	require.Len(t, response.Events, 4)
	assert.Equal(t, map[string]any{"id": "123", "payload": map[string]any{"success": true, "data": "good data"}}, response.Events[0])
	assert.Equal(t, map[string]any{"id": "456", "error": "ValueError - Bad data detected"}, response.Events[1])
	assert.Equal(t, map[string]any{"id": "789", "payload": map[string]any{"success": true, "data": "good data again"}}, response.Events[2])
	assert.Equal(t, "000", response.Events[3]["id"])
	assert.Contains(t, response.Events[3]["error"], "PayloadError - invalid payload for event 000: json: ")
}

func TestPublishWithRouter(t *testing.T) {
	t.Parallel()
	type serviceKey struct{}
	router := New()
	OnPublish(router, "/chat/*", func(ctx context.Context, payload data) (map[string]any, error) {
		service, ok := ctx.Value(serviceKey{}).(string)
		require.True(t, ok)
		return map[string]any{"from_router": true, "data": payload.Data, "service": service}, nil
	})
	app := New()
	app.Include(router)

	ctx := context.WithValue(t.Context(), serviceKey{}, "chat")
	out, err := app.Resolve(ctx, publish(t, "/chat/room/123/message", oneEvent))
	require.NoError(t, err)
	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"from_router":true,"data":"test data","service":"chat"}}]}`, string(out))
}

func TestPublishAggregate(t *testing.T) {
	t.Parallel()
	type batchProcessed struct {
		BatchProcessed bool   `json:"batch_processed"`
		Data           string `json:"data"`
	}
	app := New()
	OnPublishAggregate(app, DefaultPath, func(_ context.Context, events []Event[data]) ([]Result[batchProcessed], error) {
		results := make([]Result[batchProcessed], len(events))
		for i, ev := range events {
			results[i] = Result[batchProcessed]{ID: ev.ID, Payload: batchProcessed{BatchProcessed: true, Data: ev.Payload.Data}}
		}
		return results, nil
	})
	got := resolveJSON(t, app, publish(t, "/default/channel",
		`[{"id":"123","payload":{"data":"test data 1"}},{"id":"456","payload":{"data":"test data 2"}}]`))
	assert.JSONEq(t, `{"events":[
		{"id":"123","payload":{"batch_processed":true,"data":"test data 1"}},
		{"id":"456","payload":{"batch_processed":true,"data":"test data 2"}}]}`, got)
}

func TestPublishAggregateItemErrors(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublishAggregate(app, DefaultPath, func(_ context.Context, events []Event[data]) ([]Result[data], error) {
		results := make([]Result[data], 0, len(events))
		for _, ev := range events {
			if ev.Payload.Data == "" {
				results = append(results, Result[data]{ID: ev.ID, Error: "Error message"})
				continue
			}
			results = append(results, Result[data]{ID: ev.ID, Payload: ev.Payload})
		}
		return results, nil
	})
	got := resolveJSON(t, app, publish(t, "/default/channel",
		`[{"id":"1","payload":{}},{"id":"2","payload":{"data":"data_2"}}]`))
	// working_with_error_handling_response.json
	assert.JSONEq(t, `{"events":[{"error":"Error message","id":"1"},{"payload":{"data":"data_2"},"id":"2"}]}`, got)
}

// ChannelError is the exception of upstream's fail_entire_batch.py.
type ChannelError struct{}

func (ChannelError) Error() string { return "An exception occurred" }

func TestPublishAggregateFailsEntireBatch(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublishAggregate(app, DefaultPath, func(context.Context, []Event[data]) ([]Result[data], error) {
		return nil, ChannelError{}
	})
	OnPublishAggregate(app, "/default/typed", func(_ context.Context, events []Event[data]) ([]Result[data], error) {
		return make([]Result[data], len(events)), nil
	})

	got := resolveJSON(t, app, publish(t, "/default/channel", oneEvent))
	assert.JSONEq(t, `{"error":"ChannelError - An exception occurred"}`, got) // fail_entire_batch_response.json

	got = resolveJSON(t, app, publish(t, "/default/typed", `[{"id":"1","payload":{"data":1}}]`))
	assert.Contains(t, got, `"error":"PayloadError - invalid payload for event 1`)
}

func TestPublishAggregateEmpty(t *testing.T) {
	t.Parallel()
	for _, events := range []string{`[]`, ``, `null`, `"None"`} {
		app := New()
		calls := 0
		OnPublishAggregate(app, DefaultPath, func(_ context.Context, events []Event[data]) ([]Result[data], error) {
			calls++
			assert.Empty(t, events)
			return nil, nil
		})
		got := resolveJSON(t, app, publish(t, "/default/channel", events))
		assert.JSONEq(t, `{"events":[]}`, got, "events %q", events)
		assert.Equal(t, 1, calls)
	}
}

func TestPublishPerEventEmpty(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, DefaultPath, func(context.Context, data) (data, error) {
		t.Error("no event, no call")
		return data{}, nil
	})
	assert.JSONEq(t, `{"events":[]}`, resolveJSON(t, app, publish(t, "/default/channel", `[]`)))
}

func TestPublishNoMatchingHandler(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{w: &logs, mu: &mu}, nil))
	app := New(WithLogger(logger))
	OnPublish(app, "/matched/path", func(context.Context, data) (data, error) { return data{Data: "changed"}, nil })
	OnPublish(app, "/users/123/messages/*", func(context.Context, data) (data, error) { return data{Data: "changed"}, nil })

	for range 2 {
		got := resolveJSON(t, app, publish(t, "/unmatched/path", oneEvent))
		assert.JSONEq(t, `{"events":[{"id":"123","payload":{"data":"test data"}}]}`, got, "the events as they came")
	}
	got := resolveJSON(t, app, publish(t, "/users/123/notifications/new", oneEvent))
	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"data":"test data"}}]}`, got)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 2, "one warning per channel: %s", logs.String())
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record))
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "/unmatched/path", record["path"])
	assert.Contains(t, record["msg"], "No resolvers were found for publish operations")
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestWarningsAreBounded(t *testing.T) {
	t.Parallel()
	app := New(WithLogger(slog.New(slog.DiscardHandler)))
	for i := range maxWarnings + 10 {
		assert.True(t, app.firstWarning(fmt.Sprint("publish /p/", i)))
	}
	assert.Len(t, app.warned, maxWarnings)
	assert.False(t, app.firstWarning("publish /p/0"))
}

func TestPrecedence(t *testing.T) {
	t.Parallel()
	app := New()
	for _, path := range []string{
		"/*", "/products/*", "/products/electronics/*", "/products/electronics/phones/*",
		"/notifications/*", "/notifications/system", "/default/channel1", DefaultPath, "/a/b", "/a/b/*",
	} {
		OnPublish(app, path, func(context.Context, data) (string, error) { return path, nil })
	}
	tests := map[string]string{
		"/products/electronics/phones/123": "/products/electronics/phones/*",
		"/products/electronics/tv":         "/products/electronics/*",
		"/products/books":                  "/products/*",
		"/notifications/system":            "/notifications/system",
		"/notifications/other":             "/notifications/*",
		"/default/channel1":                "/default/channel1",
		"/default/channel2":                "/default/*",
		"/default/channel1/sub":            "/default/*",
		"/defaults/channel":                "/*",
		"/anything/here":                   "/*",
		"/a/b":                             "/a/b",
		"/a/b/c":                           "/a/b/*",
		"/a/bc":                            "/*",
	}
	for channel, want := range tests {
		got := resolveJSON(t, app, publish(t, channel, oneEvent))
		assert.JSONEq(t, `{"events":[{"id":"123","payload":`+string(mustJSON(t, want))+`}]}`, got, channel)
	}
}

func TestSubscribe(t *testing.T) {
	t.Parallel()
	app := New(WithLogger(slog.New(slog.DiscardHandler)))
	app.OnSubscribe(DefaultPath, func(ctx context.Context) error {
		ev, ok := CurrentEvent(ctx)
		require.True(t, ok)
		switch ev.Info.Channel.Path {
		case "/default/fail":
			return &ValueError{Msg: "Test error"}
		case "/default/unauthorized":
			return fmt.Errorf("token expired: %w", &UnauthorizedError{Message: "You can only subscribe to /default/foo"})
		}
		return nil
	})

	assert.JSONEq(t, `null`, resolveJSON(t, app, subscribe(t, "/default/powertools")))
	assert.JSONEq(t, `{"error":"ValueError - Test error"}`, resolveJSON(t, app, subscribe(t, "/default/fail")))
	assert.JSONEq(t, `null`, resolveJSON(t, app, subscribe(t, "/test")), "no handler accepts")
	assert.JSONEq(t, `null`, resolveJSON(t, app, load(t, "getting_started_with_testing_subscribe_event.json")))

	out, err := app.Resolve(t.Context(), subscribe(t, "/default/unauthorized"))
	assert.Nil(t, out)
	assert.Equal(t, messages.InvokeResponse_Error{
		Type: "UnauthorizedException", Message: "You can only subscribe to /default/foo",
	}, err)
}

func TestUnauthorizedPublish(t *testing.T) {
	t.Parallel()
	want := messages.InvokeResponse_Error{Type: "UnauthorizedException", Message: "Unauthorized"}

	aggregate := New()
	OnPublishAggregate(aggregate, DefaultPath, func(context.Context, []Event[data]) ([]Result[data], error) {
		return nil, &UnauthorizedError{}
	})
	_, err := aggregate.Resolve(t.Context(), publish(t, "/default/test", oneEvent))
	assert.Equal(t, want, err)

	// Per event, the first unauthorized event fails the invocation and no
	// further event is handled (Powertools Python reports it as an error of
	// that event).
	for _, concurrency := range []int{1, 3} {
		var calls atomic.Int32
		perEvent := New()
		OnPublish(perEvent, "/*", func(ctx context.Context, payload data) (data, error) {
			calls.Add(1)
			if payload.Data == "secret" {
				return data{}, &UnauthorizedError{Message: "You can only publish to /default/foo"}
			}
			<-ctx.Done()
			return payload, nil
		}, WithConcurrency(concurrency))
		_, err = perEvent.Resolve(t.Context(), publish(t, "/default/test",
			`[{"id":"1","payload":{"data":"secret"}},{"id":"2","payload":{"data":"x"}},{"id":"3","payload":{"data":"x"}},{"id":"4","payload":{"data":"x"}}]`))
		assert.Equal(t, messages.InvokeResponse_Error{
			Type: "UnauthorizedException", Message: "You can only publish to /default/foo",
		}, err, "concurrency %d", concurrency)
		assert.LessOrEqual(t, int(calls.Load()), concurrency, "no event starts after the failure")
	}
}

func TestAuthorizationControlExample(t *testing.T) {
	t.Parallel()
	// working_with_authorization_control.py
	app := New()
	OnPublish(app, "/default/foo", func(_ context.Context, payload map[string]any) (map[string]any, error) {
		return payload, nil
	})
	OnPublish(app, "/*", func(context.Context, map[string]any) (map[string]any, error) {
		return nil, &UnauthorizedError{Message: "You can only publish to /default/foo"}
	})
	app.OnSubscribe("/default/foo", func(context.Context) error { return nil })
	app.OnSubscribe("/*", func(context.Context) error {
		return &UnauthorizedError{Message: "You can only subscribe to /default/foo"}
	})

	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"data":"test data"}}]}`,
		resolveJSON(t, app, publish(t, "/default/foo", oneEvent)))
	_, err := app.Resolve(t.Context(), publish(t, "/default/bar", oneEvent))
	assert.Equal(t, messages.InvokeResponse_Error{Type: "UnauthorizedException", Message: "You can only publish to /default/foo"}, err)
	assert.JSONEq(t, `null`, resolveJSON(t, app, subscribe(t, "/default/foo")))
	_, err = app.Resolve(t.Context(), subscribe(t, "/default/bar"))
	assert.Equal(t, messages.InvokeResponse_Error{Type: "UnauthorizedException", Message: "You can only subscribe to /default/foo"}, err)
}

func TestCurrentEventAndLambdaContext(t *testing.T) {
	t.Parallel()
	router := New()
	OnPublish(router, "/router/*", func(ctx context.Context, payload data) (map[string]any, error) {
		ev, ok := CurrentEvent(ctx)
		require.True(t, ok)
		lc, ok := lambdacontext.FromContext(ctx)
		require.True(t, ok)
		return map[string]any{
			"from_router":     true,
			"function_arn":    lc.InvokedFunctionArn,
			"request_id":      lc.AwsRequestID,
			"x-forwarded-for": ev.Request.Headers["x-forwarded-for"],
			"payload_data":    payload.Data,
			"segments":        ev.Info.Channel.Segments,
			"namespace":       ev.Info.ChannelNamespace.Name,
			"events":          len(ev.Events),
			"first":           ev.Events[0].Payload,
			"identity":        ev.Identity == nil,
		}, nil
	})
	app := New()
	app.Include(router)

	ctx := lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{
		AwsRequestID:       "52fdfc07-2182-154f-163f-5f0f9a621d72",
		InvokedFunctionArn: "arn:aws:lambda:eu-west-1:809313241234:function:test-func",
	})
	out, err := app.Resolve(ctx, publish(t, "/router/test", oneEvent))
	require.NoError(t, err)
	assert.JSONEq(t, `{"events":[{"id":"123","payload":{
		"from_router":true,
		"function_arn":"arn:aws:lambda:eu-west-1:809313241234:function:test-func",
		"request_id":"52fdfc07-2182-154f-163f-5f0f9a621d72",
		"x-forwarded-for":"1.1.1.1, 2.2.2.2",
		"payload_data":"test data",
		"segments":["default","channel"],
		"namespace":"default",
		"events":1,
		"first":{"data":"test data"},
		"identity":true}}]}`, string(out))

	_, ok := CurrentEvent(t.Context())
	assert.False(t, ok, "no event outside Resolve")
}

func TestCustomRoutingPatterns(t *testing.T) {
	t.Parallel()
	type entity struct {
		EntityType   string `json:"entity_type"`
		EntityID     string `json:"entity_id"`
		ResourceType string `json:"resource_type"`
	}
	app := New()
	handler := func(kind string) func(context.Context, data) (entity, error) {
		return func(ctx context.Context, _ data) (entity, error) {
			ev, _ := CurrentEvent(ctx)
			parts := strings.Split(strings.TrimPrefix(ev.Info.Channel.Path, "/"), "/")
			return entity{EntityType: kind, EntityID: parts[1], ResourceType: parts[2]}, nil
		}
	}
	OnPublish(app, "/users/*", handler("user"))
	OnPublish(app, "/organizations/*", handler("organization"))

	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"entity_type":"user","entity_id":"123","resource_type":"posts"}}]}`,
		resolveJSON(t, app, publish(t, "/users/123/posts/456", oneEvent)))
	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"entity_type":"organization","entity_id":"abc","resource_type":"members"}}]}`,
		resolveJSON(t, app, publish(t, "/organizations/abc/members/xyz", oneEvent)))
}

func TestComplexRouterHierarchy(t *testing.T) {
	t.Parallel()
	base, users, profiles := New(), New(), New()
	OnPublish(base, "/api/*", func(context.Context, data) (string, error) { return "base", nil })
	OnPublish(users, "/api/v1/users/*", func(context.Context, data) (string, error) { return "users", nil })
	OnPublish(profiles, "/api/v1/users/profile/*", func(context.Context, data) (string, error) { return "profiles", nil })
	app := New()
	app.Include(base)
	app.Include(users)
	app.Include(profiles)
	app.OnSubscribe("/api/*", func(context.Context) error { return nil })

	assert.JSONEq(t, `{"events":[{"id":"123","payload":"profiles"}]}`,
		resolveJSON(t, app, publish(t, "/api/v1/users/profile/update", oneEvent)))
	assert.JSONEq(t, `{"events":[{"id":"123","payload":"users"}]}`,
		resolveJSON(t, app, publish(t, "/api/v1/users/list", oneEvent)))
}

func TestConcurrentEvents(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, DefaultPath, func(_ context.Context, payload struct {
		Delay int `json:"delay"`
	},
	) (int, error) {
		time.Sleep(time.Duration(payload.Delay) * time.Millisecond)
		return payload.Delay, nil
	}, WithConcurrency(3))

	start := time.Now()
	got := resolveJSON(t, app, publish(t, "/default/concurrent",
		`[{"id":"123","payload":{"delay":60}},{"id":"456","payload":{"delay":30}},{"id":"789","payload":{"delay":45}}]`))
	assert.Less(t, time.Since(start), 130*time.Millisecond, "the events run at the same time")
	assert.JSONEq(t, `{"events":[{"id":"123","payload":60},{"id":"456","payload":30},{"id":"789","payload":45}]}`, got,
		"the response keeps the order of the events")
}

func TestConcurrentPanicPropagates(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, DefaultPath, func(_ context.Context, payload data) (data, error) {
		if payload.Data == "panic" {
			panic("handler bug")
		}
		return payload, nil
	}, WithConcurrency(2))
	ev := publish(t, "/default/x", `[{"id":"1","payload":{"data":"a"}},{"id":"2","payload":{"data":"panic"}}]`)
	assert.PanicsWithValue(t, "handler bug", func() {
		_, _ = app.Resolve(t.Context(), ev) //nolint:errcheck // the property is the panic
	})
}

func TestInvalidPaths(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, data) (data, error) { return data{}, nil }
	for _, path := range []string{
		"", "/", "users", "default/*", "/users/*/notifications/*", "/*/*/*", "/default/channel*",
		"/default/*/subpath", "//", "/a//b", "/a/", "/a/**", "/**", "*",
	} {
		assert.Panics(t, func() { OnPublish(New(), path, noop) }, "path %q", path)
		assert.Panics(t, func() { New().OnSubscribe(path, func(context.Context) error { return nil }) }, "path %q", path)
	}
}

func TestDuplicateRegistrations(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, data) (data, error) { return data{}, nil }
	aggregate := func(context.Context, []Event[data]) ([]Result[data], error) { return nil, nil }
	subscribeNoop := func(context.Context) error { return nil }
	tests := map[string]func(){
		"publish": func() {
			app := New()
			OnPublish(app, "/default/duplicate", noop)
			OnPublishAggregate(app, "/default/duplicate", aggregate)
		},
		"subscribe": func() {
			app := New()
			app.OnSubscribe("/default/duplicate", subscribeNoop)
			app.OnSubscribe("/default/duplicate", subscribeNoop)
		},
		"include publish": func() {
			app, router := New(), New()
			OnPublish(app, "/default/duplicate", noop)
			OnPublish(router, "/default/duplicate", noop)
			app.Include(router)
		},
		"include subscribe": func() {
			app, router := New(), New()
			app.OnSubscribe("/*", subscribeNoop)
			router.OnSubscribe("/*", subscribeNoop)
			app.Include(router)
		},
	}
	for name, register := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Panics(t, register)
		})
	}

	// A publish and a subscribe handler for the same path coexist.
	app := New()
	OnPublish(app, "/default/duplicate", noop)
	app.OnSubscribe("/default/duplicate", subscribeNoop)
}

func TestInvalidEvents(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, "/*", func(_ context.Context, payload data) (data, error) { return payload, nil })
	for _, payload := range []string{
		``, `null`, `[]`, `"text"`, `{"info":"x"}`, `{"info":{"operation":"PUBLISH"},"events":[1]}`,
		`{"info":{"operation":"PUBLISH"},"events":[{"id":1}]}`,
	} {
		_, err := app.Resolve(t.Context(), jsontext.Value(payload))
		var invalid *InvalidEventError
		assert.ErrorAs(t, err, &invalid, "payload %q", payload)
	}
}

func TestLenientEvent(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, "/*", func(ctx context.Context, payload data) (bool, error) {
		ev, _ := CurrentEvent(ctx)
		return ev.Identity == nil && ev.Prev != nil && payload.Data == "", nil
	})
	got := resolveJSON(t, app, jsontext.Value(`{"identity":"None","prev":"None","result":"None","error":"None",`+
		`"info":{"operation":"PUBLISH","channel":{"path":"/default/channel"}},"events":[{"id":"1"}]}`))
	assert.JSONEq(t, `{"events":[{"id":"1","payload":true}]}`, got)
}

func TestResultMarshal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		result any
		want   string
	}{
		{Result[data]{ID: "1", Payload: data{Data: "x"}}, `{"id":"1","payload":{"data":"x"}}`},
		{Result[data]{ID: "1", Error: "boom"}, `{"id":"1","error":"boom"}`},
		{Result[*data]{ID: "1"}, `{"id":"1","payload":null}`},
		{Result[int]{ID: "1"}, `{"id":"1","payload":0}`},
	}
	for _, tt := range tests {
		assert.JSONEq(t, tt.want, string(mustJSON(t, tt.result)))
	}
	_, err := json.Marshal(Result[func()]{ID: "1", Payload: func() {}})
	require.ErrorContains(t, err, "encode the result of event 1")
}

func TestPerEventEncodingError(t *testing.T) {
	t.Parallel()
	app := New()
	OnPublish(app, "/*", func(context.Context, data) (func(), error) { return func() {}, nil })
	got := resolveJSON(t, app, publish(t, "/default/x", oneEvent))
	assert.Contains(t, got, `"error":"wrapError - appsyncevents: encode the payload of event 123`)
}

func TestZeroRouterAndNilOptions(t *testing.T) {
	t.Parallel()
	var app Router
	OnPublish(&app, "/*", func(_ context.Context, payload data) (data, error) { return payload, nil }, nil)
	assert.JSONEq(t, `{"events":[{"id":"123","payload":{"data":"test data"}}]}`,
		resolveJSON(t, &app, publish(t, "/default/x", oneEvent)))
	assert.NotNil(t, New(nil))
}

func TestDescribe(t *testing.T) {
	t.Parallel()
	type unnamed = struct{ error }
	tests := []struct {
		err  error
		want string
	}{
		{&ValueError{Msg: "m"}, "ValueError - m"},
		{NotAuthorizedError{Msg: "m"}, "NotAuthorizedError - m"},
		{messages.InvokeResponse_Error{Type: "T", Message: "m"}, "T - m"},
		{fmt.Errorf("w: %w", &ValueError{Msg: "m"}), "wrapError - w: m"},
		{unnamed{errors.New("m")}, "Error - m"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, describe(tt.err))
	}
}
