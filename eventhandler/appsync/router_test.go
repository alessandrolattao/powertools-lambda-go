package appsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// load reads a sample event from testdata; most come from Powertools
// Python's tests/events and the examples of its documentation.
func load(t *testing.T, name string) jsontext.Value {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

// resolve runs app on event and decodes the response into T.
func resolve[T any](t *testing.T, app *Router, event jsontext.Value) T {
	t.Helper()
	out, err := app.Resolve(t.Context(), event)
	require.NoError(t, err)
	var got T
	require.NoError(t, json.Unmarshal(out, &got), "response %s", out)
	return got
}

type idArgs struct {
	ID string `json:"id"`
}

type locationArgs struct {
	Page        int    `json:"page"`
	Size        int    `json:"size"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ValueError stands for the Python exception of the upstream tests.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

func TestDirectResolver(t *testing.T) {
	t.Parallel()
	for _, typeName := range []string{AnyType, "Mutation"} {
		t.Run(typeName, func(t *testing.T) {
			t.Parallel()
			app := New()
			Resolver(app, typeName, "createSomething", func(_ context.Context, args idArgs) (string, error) {
				return args.ID, nil
			})
			assert.Equal(t, "my identifier", resolve[string](t, app, load(t, "appSyncDirectResolver.json")))
		})
	}
}

func TestCustomResolverWithFields(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, "Merchant", "locations", func(_ context.Context, args locationArgs) (int, error) {
		return args.Page, nil
	})
	assert.Equal(t, 2, resolve[int](t, app, load(t, "appSyncCustomResolverEvent.json")))
}

func TestAmplifyResolver(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, "Merchant", "locations", func(ctx context.Context, args locationArgs) (string, error) {
		ev, ok := CurrentEvent(ctx)
		require.True(t, ok)
		assert.Equal(t, "Merchant", ev.ParentType())
		assert.Equal(t, 2, args.Page)
		assert.Equal(t, 1, args.Size)
		assert.InDelta(t, 2.0, ev.Arguments["page"], 0)
		return args.Name, nil
	})
	assert.Equal(t, "value", resolve[string](t, app, load(t, "appSyncResolverEvent.json")))
}

func TestResolverWithoutArguments(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, "Query", "noParams", func(context.Context, struct{}) (string, error) {
		return "no_params has no params", nil
	})
	for _, event := range []string{
		`{"typeName":"Query","fieldName":"noParams","arguments":{}}`,
		`{"typeName":"Query","fieldName":"noParams","arguments":null}`,
		`{"typeName":"Query","fieldName":"noParams"}`,
	} {
		assert.Equal(t, "no_params has no params", resolve[string](t, app, jsontext.Value(event)), event)
	}
}

func TestResolverNotFound(t *testing.T) {
	t.Parallel()
	app := New()
	BatchResolver(app, "type", "field", func(_ context.Context, events []Event[struct{}]) ([]int, error) {
		return make([]int, len(events)), nil
	})
	_, err := app.Resolve(t.Context(), jsontext.Value(`{"typeName":"type","fieldName":"field","arguments":{}}`))
	var notFound *ResolverNotFoundError
	require.ErrorAs(t, err, &notFound)
	assert.Equal(t, "No resolver found for 'type.field'", err.Error())
	assert.False(t, notFound.Batch)
}

func TestResolverMultipleMappings(t *testing.T) {
	t.Parallel()
	app := New()
	getLocations := func(_ context.Context, args locationArgs) (string, error) {
		return args.Name + args.Description, nil
	}
	Resolver(app, AnyType, "listLocations", getLocations)
	Resolver(app, AnyType, "locations", getLocations)

	assert.Equal(t, "value", resolve[string](t, app,
		jsontext.Value(`{"typeName":"Query","fieldName":"listLocations","arguments":{"name":"value"}}`)))
	assert.Equal(t, "value2description", resolve[string](t, app,
		jsontext.Value(`{"typeName":"Merchant","fieldName":"locations","arguments":{"name":"value2","description":"description"}}`)))
}

func TestExactTypeWinsOverAnyType(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, AnyType, "field", func(context.Context, struct{}) (string, error) { return "any", nil })
	Resolver(app, "Query", "field", func(context.Context, struct{}) (string, error) { return "query", nil })

	assert.Equal(t, "query", resolve[string](t, app, jsontext.Value(`{"typeName":"Query","fieldName":"field"}`)))
	assert.Equal(t, "any", resolve[string](t, app, jsontext.Value(`{"typeName":"Other","fieldName":"field"}`)))
}

// customModel is a data model of one's own, Powertools Python's data_model.
type customModel struct {
	eventsource.AppSyncResolverEvent
}

func (m customModel) CountryViewer() string {
	return m.Request.Headers["cloudfront-viewer-country"]
}

func TestDecodeEventCustomModel(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, AnyType, "createSomething", func(ctx context.Context, args idArgs) (string, error) {
		model, err := DecodeEvent[customModel](ctx)
		if err != nil {
			return "", err
		}
		return args.ID + "@" + model.CountryViewer(), nil
	})
	assert.Equal(t, "my identifier@US", resolve[string](t, app, load(t, "appSyncDirectResolver.json")))

	_, err := DecodeEvent[customModel](t.Context())
	require.ErrorIs(t, err, ErrNoEvent)
}

func TestIncludeRouter(t *testing.T) {
	t.Parallel()
	app := New()
	router := New()
	Resolver(router, "Query", "listLocations", func(_ context.Context, args locationArgs) (string, error) {
		return "get_locations#" + args.Name, nil
	})
	Resolver(app, AnyType, "listLocations2", func(_ context.Context, args locationArgs) (string, error) {
		return "get_locations2#" + args.Name, nil
	})
	app.Include(router)

	assert.Equal(t, "get_locations#value", resolve[string](t, app,
		jsontext.Value(`{"typeName":"Query","fieldName":"listLocations","arguments":{"name":"value"}}`)))
	assert.Equal(t, "get_locations2#value", resolve[string](t, app,
		jsontext.Value(`{"typeName":"Query","fieldName":"listLocations2","arguments":{"name":"value"}}`)))
}

type adminKey struct{}

// TestContextValues is Powertools Python's append_context: data stored in
// the context before Resolve reaches the resolvers of every router, and
// nothing outlives the invocation.
func TestContextValues(t *testing.T) {
	t.Parallel()
	app := New()
	router := New()
	Resolver(router, "Query", "listLocations", func(ctx context.Context, args locationArgs) (*string, error) {
		var result *string // null for everyone but admins
		if admin, ok := ctx.Value(adminKey{}).(bool); ok && admin {
			result = new("get_locations#" + args.Name)
		}
		return result, nil
	})
	app.Include(router)
	event := jsontext.Value(`{"typeName":"Query","fieldName":"listLocations","arguments":{"name":"value"}}`)

	out, err := app.Resolve(context.WithValue(t.Context(), adminKey{}, true), event)
	require.NoError(t, err)
	assert.JSONEq(t, `"get_locations#value"`, string(out))

	out, err = app.Resolve(t.Context(), event)
	require.NoError(t, err)
	assert.JSONEq(t, `null`, string(out))

	_, ok := CurrentEvent(t.Context())
	assert.False(t, ok, "no event outside Resolve")
}

func TestCurrentEventAndLambdaContext(t *testing.T) {
	t.Parallel()
	event := load(t, "appSyncDirectResolver.json")
	app := New()
	router := New()
	Resolver(router, AnyType, "createSomething", func(ctx context.Context, _ idArgs) (string, error) {
		ev, ok := CurrentEvent(ctx)
		require.True(t, ok)
		_, isBatch := CurrentBatch(ctx)
		assert.False(t, isBatch)
		lc, ok := lambdacontext.FromContext(ctx)
		require.True(t, ok)
		assert.Equal(t, "Mutation", ev.ParentType())
		assert.Equal(t, "my identifier", ev.Arguments["id"])
		assert.Equal(t, "US", ev.Request.Headers["cloudfront-viewer-country"])
		return ev.Identity.Sub + "/" + lc.AwsRequestID, nil
	})
	app.Include(router)

	ctx := lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{AwsRequestID: "req-1"})
	out, err := app.Resolve(ctx, event)
	require.NoError(t, err)
	assert.JSONEq(t, `"192879fc-a240-4bf1-ab5a-d6a00f3063f9/req-1"`, string(out))
}

func TestOnErrorSingleResolver(t *testing.T) {
	t.Parallel()
	app := New()
	OnError(app, func(ctx context.Context, err *ValueError) (map[string]string, error) {
		ev, ok := CurrentEvent(ctx)
		require.True(t, ok, "the handler sees the event")
		return map[string]string{"message": "error", "cause": err.Msg, "field": ev.Field()}, nil
	})
	Resolver(app, AnyType, "createSomething", func(context.Context, idArgs) (string, error) {
		return "", &ValueError{Msg: "Error"}
	})
	got := resolve[map[string]string](t, app, load(t, "appSyncDirectResolver.json"))
	assert.Equal(t, map[string]string{"message": "error", "cause": "Error", "field": "createSomething"}, got)
}

func TestOnErrorPrecedence(t *testing.T) {
	t.Parallel()
	errHandler := errors.New("handler failed")
	app := New()
	OnError(app, func(context.Context, *ValueError) (string, error) { return "value", nil })
	router := New()
	OnError(router, func(context.Context, *ResolverNotFoundError) (string, error) { return "not found", nil })
	app.Include(router) // the router's handlers come after the app's
	OnError(app, func(context.Context, error) (string, error) { return "", errHandler })

	var failure error
	Resolver(app, "Query", "f", func(context.Context, idArgs) (string, error) { return "", failure })

	tests := []struct {
		name    string
		event   string
		failure error
		want    string
	}{
		{name: "first match", event: `{"typeName":"Query","fieldName":"f"}`, failure: &ValueError{Msg: "x"}, want: `"value"`},
		{
			name: "match in the chain", event: `{"typeName":"Query","fieldName":"f"}`,
			failure: errors.Join(errors.New("context"), &ValueError{Msg: "x"}), want: `"value"`,
		},
		{name: "included handler", event: `{"typeName":"Query","fieldName":"missing"}`, want: `"not found"`},
		{name: "catch-all handler fails", event: `{"typeName":"Query","fieldName":"f","arguments":{"id":1}}`},
	}
	for _, tt := range tests {
		failure = tt.failure
		out, err := app.Resolve(t.Context(), jsontext.Value(tt.event))
		if tt.want == "" {
			require.ErrorIs(t, err, errHandler, tt.name)
			continue
		}
		require.NoError(t, err, tt.name)
		assert.JSONEq(t, tt.want, string(out), tt.name)
	}
}

func TestUnhandledErrorIsReturnedUnwrapped(t *testing.T) {
	t.Parallel()
	failure := &ValueError{Msg: "boom"}
	app := New()
	Resolver(app, "Query", "f", func(context.Context, struct{}) (string, error) { return "", failure })
	_, err := app.Resolve(t.Context(), jsontext.Value(`{"typeName":"Query","fieldName":"f"}`))
	assert.Same(t, failure, err) // aws-lambda-go names the errorType after this very type
}

func TestArgumentsError(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, "Query", "getTodo", func(_ context.Context, args idArgs) (string, error) { return args.ID, nil })
	_, err := app.Resolve(t.Context(), jsontext.Value(`{"typeName":"Query","fieldName":"getTodo","arguments":{"id":42}}`))
	var argsErr *ArgumentsError
	require.ErrorAs(t, err, &argsErr)
	assert.Equal(t, "Query", argsErr.TypeName)
	assert.Equal(t, "getTodo", argsErr.FieldName)
	assert.Contains(t, err.Error(), "invalid arguments for 'Query.getTodo'")
}

func TestInvalidEvent(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, AnyType, "f", func(context.Context, struct{}) (int, error) { return 1, nil })
	for _, payload := range []string{``, `null`, `42`, `"text"`, `{"info":"x"}`, `[1]`, `[{"info":{"fieldName":1}}]`} {
		_, err := app.Resolve(t.Context(), jsontext.Value(payload))
		var invalid *InvalidEventError
		assert.ErrorAs(t, err, &invalid, "payload %q", payload)
	}
}

func TestLenientIdentity(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, AnyType, "f", func(ctx context.Context, _ struct{}) (bool, error) {
		ev, _ := CurrentEvent(ctx)
		return ev.Identity == nil, nil
	})
	for _, identity := range []string{`"None"`, `null`, `42`} {
		event := `{"typeName":"Query","fieldName":"f","identity":` + identity + `,"source":"None","prev":"None"}`
		assert.True(t, resolve[bool](t, app, jsontext.Value(event)), event)
	}
}

func TestResultEncoding(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, AnyType, "raw", func(context.Context, struct{}) (jsontext.Value, error) {
		return jsontext.Value(`{"kept":[1,2,3]}`), nil
	})
	Resolver(app, AnyType, "bad", func(context.Context, struct{}) (func(), error) { return func() {}, nil })

	out, err := app.Resolve(t.Context(), jsontext.Value(`{"typeName":"Query","fieldName":"raw"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"kept":[1,2,3]}`, string(out))

	_, err = app.Resolve(t.Context(), jsontext.Value(`{"typeName":"Query","fieldName":"bad"}`))
	require.ErrorContains(t, err, "encode the result of 'Query.bad'")
}

func TestDecodeSource(t *testing.T) {
	t.Parallel()
	type merchant struct {
		Name   string `json:"name"`
		Nested struct {
			Name string `json:"name"`
		} `json:"nested"`
		Big int64 `json:"big"`
	}
	app := New()
	Resolver(app, "Merchant", "locations", func(ctx context.Context, _ locationArgs) (merchant, error) {
		ev, _ := CurrentEvent(ctx)
		return ev.DecodeSource[merchant]()
	})
	event := `{"typeName":"Merchant","fieldName":"locations","source":{"name":"Value","nested":{"name":"value"},"big":9007199254740993}}`
	got := resolve[merchant](t, app, jsontext.Value(event))
	assert.Equal(t, "Value", got.Name)
	assert.Equal(t, "value", got.Nested.Name)
	assert.Equal(t, int64(9007199254740993), got.Big, "the raw source keeps every digit")

	// An event built by hand, as in the tests of a batch resolver, decodes
	// its Source map.
	byHand := Event[struct{}]{Source: map[string]any{"name": "x"}}
	m, err := byHand.DecodeSource[merchant]()
	require.NoError(t, err)
	assert.Equal(t, "x", m.Name)

	// No source (a root field) is the zero value.
	m, err = Event[struct{}]{}.DecodeSource[merchant]()
	require.NoError(t, err)
	assert.Equal(t, merchant{}, m)

	_, err = Event[struct{}]{source: jsontext.Value(`"text"`)}.DecodeSource[merchant]()
	require.ErrorContains(t, err, "decode the source")
}

func TestRegistrationPanics(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, struct{}) (int, error) { return 0, nil }
	batchNoop := func(context.Context, []Event[struct{}]) ([]int, error) { return nil, nil }
	tests := map[string]func(){
		"empty field":       func() { Resolver(New(), "Query", "", noop) },
		"empty type":        func() { Resolver(New(), "", "f", noop) },
		"empty batch field": func() { BatchResolver(New(), AnyType, "", batchNoop) },
		"duplicate": func() {
			app := New()
			Resolver(app, "Query", "f", noop)
			Resolver(app, "Query", "f", noop)
		},
		"duplicate batch": func() {
			app := New()
			BatchResolver(app, "Query", "f", batchNoop)
			BatchItemResolver(app, "Query", "f", func(context.Context, Event[struct{}]) (int, error) { return 0, nil })
		},
		"include conflict": func() {
			app, router := New(), New()
			Resolver(app, "Query", "f", noop)
			Resolver(router, "Query", "f", noop)
			app.Include(router)
		},
		"include batch conflict": func() {
			app, router := New(), New()
			BatchResolver(app, "Query", "f", batchNoop)
			BatchResolver(router, "Query", "f", batchNoop)
			app.Include(router)
		},
	}
	for name, register := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Panics(t, register)
		})
	}

	// A single resolver and a batch resolver for the same field coexist.
	app := New()
	Resolver(app, "Query", "f", noop)
	BatchResolver(app, "Query", "f", batchNoop)
}

func TestZeroRouter(t *testing.T) {
	t.Parallel()
	var app Router
	Resolver(&app, "Query", "f", func(context.Context, struct{}) (int, error) { return 7, nil })
	assert.Equal(t, 7, resolve[int](t, &app, jsontext.Value(`{"typeName":"Query","fieldName":"f"}`)))
}

func TestGettingStartedEvents(t *testing.T) {
	t.Parallel()
	type todo struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Completed bool   `json:"completed"`
	}
	app := New()
	Resolver(app, "Query", "getTodo", func(_ context.Context, args idArgs) (todo, error) {
		return todo{ID: args.ID, Title: "delectus aut autem"}, nil
	})
	Resolver(app, "Query", "listTodos", func(context.Context, struct{}) ([]todo, error) {
		return []todo{{ID: "1"}, {ID: "2"}}, nil
	})
	Resolver(app, "Mutation", "createTodo", func(_ context.Context, args struct {
		Title string `json:"title"`
	},
	) (todo, error) {
		return todo{ID: "new", Title: args.Title}, nil
	})

	got := resolve[todo](t, app, load(t, "getting_started_get_todo.json"))
	assert.Equal(t, todo{ID: "7e362732-c8cd-4405-b090-144ac9b38960", Title: "delectus aut autem"}, got)
	assert.Len(t, resolve[[]todo](t, app, load(t, "getting_started_list_todos.json")), 2)
	created := resolve[todo](t, app, load(t, "getting_started_create_todo.json"))
	assert.Equal(t, "new", created.ID)
	assert.NotEmpty(t, created.Title)
}

func TestGraphQLTransformerEvent(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, "Query", "listLocations", func(ctx context.Context, args locationArgs) ([]map[string]any, error) {
		ev, _ := CurrentEvent(ctx)
		return []map[string]any{{"name": args.Name, "page": args.Page, "country": ev.Request.Headers["cloudfront-viewer-country"]}}, nil
	})
	got := resolve[[]map[string]any](t, app, load(t, "graphql_transformer_list_locations.json"))
	require.Len(t, got, 1)
}

// TestMiddleware wraps Resolve in the middleware package's decorators, as
// a function does with the logger, the tracer and metrics.
func TestMiddleware(t *testing.T) {
	t.Parallel()
	app := New()
	Resolver(app, "Query", "f", func(ctx context.Context, _ struct{}) (bool, error) {
		admin, ok := ctx.Value(adminKey{}).(bool)
		return ok && admin, nil
	})
	inject := func(next middleware.Handler[jsontext.Value, jsontext.Value]) middleware.Handler[jsontext.Value, jsontext.Value] {
		return func(ctx context.Context, event jsontext.Value) (jsontext.Value, error) {
			return next(context.WithValue(ctx, adminKey{}, true), event)
		}
	}
	var seen string
	record := middleware.After(func(_ context.Context, _, result jsontext.Value, err error) (jsontext.Value, error) {
		seen = string(result)
		return result, err
	})
	handler := middleware.Chain(app.Resolve, record, inject)

	out, err := handler(t.Context(), jsontext.Value(`{"typeName":"Query","fieldName":"f"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `true`, string(out))
	assert.Equal(t, "true", seen)
}
