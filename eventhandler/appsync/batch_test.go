package appsync

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relatedPostsEvent is the batch of Powertools Python's
// test_resolve_batch_processing_with_related_events_one_at_time.
const relatedPostsEvent = `[
	{"arguments":{},"identity":"None","source":{"post_id":"3","title":"Third book"},
	 "info":{"selectionSetList":["title"],"selectionSetGraphQL":"{\n  title\n}","fieldName":"relatedPosts","parentTypeName":"Post"}},
	{"arguments":{},"identity":"None","source":{"post_id":"4","title":"Fifth book"},
	 "info":{"selectionSetList":["title"],"selectionSetGraphQL":"{\n  title\n}","fieldName":"relatedPosts","parentTypeName":"Post"}},
	{"arguments":{},"identity":"None","source":{"post_id":"1","title":"First book"},
	 "info":{"selectionSetList":["title"],"selectionSetGraphQL":"{\n  title\n}","fieldName":"relatedPosts","parentTypeName":"Post"}}
]`

// listLocationsEvent is the batch most upstream batch tests use: three
// requests for Post.listLocations whose sources carry an id.
const listLocationsEvent = `[
	{"typeName":"Query","info":{"fieldName":"listLocations","parentTypeName":"Post"},"fieldName":"listLocations","arguments":{},"source":{"id":"1"}},
	{"typeName":"Query","info":{"fieldName":"listLocations","parentTypeName":"Post"},"fieldName":"listLocations","arguments":{},"source":{"id":"2"}},
	{"typeName":"Query","info":{"fieldName":"listLocations","parentTypeName":"Post"},"fieldName":"listLocations","arguments":{},"source":{"id":[3,4]}}
]`

type post struct {
	PostID string `json:"post_id"`
	Title  string `json:"title"`
}

func TestBatchItemResolverRelatedPosts(t *testing.T) {
	t.Parallel()
	posts := map[string]post{
		"1": {"1", "First book"}, "2": {"2", "Second book"}, "3": {"3", "Third book"}, "4": {"4", "Fourth book"},
	}
	related := map[string][]post{
		"1": {posts["2"]},
		"2": {posts["3"], posts["4"], posts["1"]},
		"3": {posts["2"], posts["1"]},
		"4": {posts["3"], posts["1"]},
	}
	app := New()
	BatchItemResolver(app, "Post", "relatedPosts", func(_ context.Context, ev Event[struct{}]) ([]post, error) {
		parent, err := ev.DecodeSource[post]()
		return related[parent.PostID], err
	})
	got := resolve[[][]post](t, app, jsontext.Value(relatedPostsEvent))
	assert.Equal(t, [][]post{related["3"], related["4"], related["1"]}, got)
}

func TestBatchItemResolverSimpleQueries(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{1, 3} {
		t.Run(fmt.Sprint("concurrency ", concurrency), func(t *testing.T) {
			t.Parallel()
			app := New()
			BatchItemResolver(app, AnyType, "listLocations", func(ctx context.Context, ev Event[struct{}]) (any, error) {
				batch, ok := CurrentBatch(ctx)
				require.True(t, ok)
				assert.Len(t, batch, 3)
				_, single := CurrentEvent(ctx)
				assert.False(t, single)
				return ev.Source["id"], nil
			}, WithConcurrency(concurrency))
			got := resolve[[]any](t, app, jsontext.Value(listLocationsEvent))
			assert.Equal(t, []any{"1", "2", []any{3.0, 4.0}}, got)
		})
	}
}

func TestBatchItemResolverRaiseOnError(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{1, 2} {
		t.Run(fmt.Sprint("concurrency ", concurrency), func(t *testing.T) {
			t.Parallel()
			failure := &ValueError{Msg: "boom"}
			app := New()
			BatchItemResolver(app, AnyType, "listLocations", func(context.Context, Event[struct{}]) (string, error) {
				return "", failure
			}, WithRaiseOnError(true), WithConcurrency(concurrency))
			_, err := app.Resolve(t.Context(), jsontext.Value(listLocationsEvent))
			assert.Same(t, failure, err)
		})
	}
}

func TestBatchItemResolverWithoutRaise(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{1, 2} {
		t.Run(fmt.Sprint("concurrency ", concurrency), func(t *testing.T) {
			t.Parallel()
			app := New()
			BatchItemResolver(app, AnyType, "listLocations", func(context.Context, Event[struct{}]) (string, error) {
				return "", &ValueError{Msg: "boom"}
			}, WithRaiseOnError(false), WithConcurrency(concurrency))
			out, err := app.Resolve(t.Context(), jsontext.Value(listLocationsEvent))
			require.NoError(t, err)
			assert.JSONEq(t, `[null,null,null]`, string(out))
		})
	}
}

func TestBatchItemResolverPartialFailures(t *testing.T) {
	t.Parallel()
	app := New()
	BatchItemResolver(app, AnyType, "listLocations", func(_ context.Context, ev Event[idArgs]) (string, error) {
		if ev.Arguments.ID == "fail" {
			return "", errors.New("failed")
		}
		return "ok:" + ev.Arguments.ID, nil
	})
	event := `[
		{"info":{"fieldName":"listLocations","parentTypeName":"Query"},"arguments":{"id":"a"}},
		{"info":{"fieldName":"listLocations","parentTypeName":"Query"},"arguments":{"id":"fail"}},
		{"info":{"fieldName":"listLocations","parentTypeName":"Query"},"arguments":{"id":42}},
		{"info":{"fieldName":"listLocations","parentTypeName":"Query"},"arguments":{"id":"b"}}
	]`
	out, err := app.Resolve(t.Context(), jsontext.Value(event))
	require.NoError(t, err)
	assert.JSONEq(t, `["ok:a",null,null,"ok:b"]`, string(out), "errors and bad arguments are null")
}

func TestBatchResolverNotFound(t *testing.T) {
	t.Parallel()
	app := New()
	router := New()
	Resolver(router, "Query", "listLocations", func(context.Context, struct{}) (string, error) { return "", nil })
	BatchItemResolver(router, "Query", "listCars", func(context.Context, Event[struct{}]) (string, error) { return "", nil })
	app.Include(router)

	_, err := app.Resolve(t.Context(), jsontext.Value(`[{"typeName":"Query","fieldName":"listLocations","arguments":{}}]`))
	var notFound *ResolverNotFoundError
	require.ErrorAs(t, err, &notFound)
	assert.True(t, notFound.Batch)
	assert.Equal(t, "No resolver found for 'Query.listLocations'", err.Error())
}

func TestBatchResolverWithRouter(t *testing.T) {
	t.Parallel()
	app := New()
	router := New()
	BatchItemResolver(router, "Query", "listLocations", func(_ context.Context, ev Event[locationArgs]) (string, error) {
		return "get_locations#" + ev.Arguments.Name + "#" + fmt.Sprint(ev.Source["id"]), nil
	})
	BatchItemResolver(router, AnyType, "listLocations2", func(_ context.Context, ev Event[locationArgs]) (string, error) {
		return "get_locations2#" + ev.Arguments.Name + "#" + fmt.Sprint(ev.Source["id"]), nil
	}, WithConcurrency(4))
	app.Include(router)

	got1 := resolve[[]string](t, app, jsontext.Value(
		`[{"typeName":"Query","fieldName":"listLocations","arguments":{"name":"value"},"source":{"id":"1"}}]`))
	got2 := resolve[[]string](t, app, jsontext.Value(
		`[{"typeName":"Query","fieldName":"listLocations2","arguments":{"name":"value"},"source":{"id":"2"}}]`))
	assert.Equal(t, []string{"get_locations#value#1"}, got1)
	assert.Equal(t, []string{"get_locations2#value#2"}, got2)
}

func TestSingleAndBatchResolverForOneField(t *testing.T) {
	t.Parallel()
	app := New()
	BatchItemResolver(app, "Query", "listLocations", func(_ context.Context, ev Event[locationArgs]) (string, error) {
		return "get_locations#" + ev.Arguments.Name + "#" + fmt.Sprint(ev.Source["id"]), nil
	}, WithConcurrency(2))
	Resolver(app, AnyType, "listLocations", func(_ context.Context, args locationArgs) (string, error) {
		return "get_location#" + args.Name, nil
	})
	got1 := resolve[[]string](t, app, jsontext.Value(
		`[{"typeName":"Query","fieldName":"listLocations","arguments":{"name":"value"},"source":{"id":"1"}}]`))
	got2 := resolve[string](t, app, jsontext.Value(
		`{"typeName":"Query","fieldName":"listLocations","arguments":{"name":"value"},"source":{"id":"1"}}`))
	assert.Equal(t, []string{"get_locations#value#1"}, got1)
	assert.Equal(t, "get_location#value", got2)
}

func TestBatchResolverAggregate(t *testing.T) {
	t.Parallel()
	app := New()
	BatchResolver(app, AnyType, "listLocations", func(ctx context.Context, events []Event[struct{}]) ([]any, error) {
		batch, ok := CurrentBatch(ctx)
		require.True(t, ok)
		assert.Len(t, batch, len(events))
		results := make([]any, len(events))
		for i, ev := range events {
			results[i] = ev.Source["id"]
		}
		return results, nil
	})
	got := resolve[[]any](t, app, jsontext.Value(listLocationsEvent))
	assert.Equal(t, []any{"1", "2", []any{3.0, 4.0}}, got)
}

func TestBatchResolverAdvancedPayload(t *testing.T) {
	t.Parallel()
	related := map[string]map[string]string{"1": {"title": "post1"}, "2": {"title": "post2"}, "3": {"title": "post3"}}
	app := New()
	BatchResolver(app, "Post", "relatedPosts", func(_ context.Context, events []Event[struct{}]) ([]map[string]string, error) {
		results := make([]map[string]string, len(events))
		for i, ev := range events {
			parent, err := ev.DecodeSource[post]()
			if err != nil {
				return nil, err
			}
			results[i] = related[parent.PostID]
		}
		return results, nil
	})
	got := resolve[[]map[string]string](t, app, load(t, "advanced_batch_resolver_payload.json"))
	assert.Equal(t, []map[string]string{related["1"], related["2"], related["1"]}, got)
}

func TestBatchResolverInvalidResponse(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{"no results (Python's None)": nil, "too few": {"a"}, "too many": {"a", "b", "c", "d"}}
	for name, results := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := New()
			BatchResolver(app, AnyType, "listLocations", func(context.Context, []Event[struct{}]) ([]string, error) {
				return results, nil
			})
			_, err := app.Resolve(t.Context(), jsontext.Value(listLocationsEvent))
			var invalid *InvalidBatchResponseError
			require.ErrorAs(t, err, &invalid)
			assert.Equal(t, 3, invalid.Items)
			assert.Equal(t, len(results), invalid.Results)
			assert.Equal(t, "Post", invalid.TypeName)
			assert.Contains(t, err.Error(), "'Post.listLocations' returned")
		})
	}
}

func TestBatchResolverAggregateErrors(t *testing.T) {
	t.Parallel()
	failure := &ValueError{Msg: "boom"}
	app := New()
	BatchResolver(app, AnyType, "fails", func(context.Context, []Event[struct{}]) ([]string, error) { return nil, failure })
	BatchResolver(app, AnyType, "typed", func(_ context.Context, events []Event[idArgs]) ([]string, error) {
		return make([]string, len(events)), nil
	})

	_, err := app.Resolve(t.Context(), jsontext.Value(`[{"fieldName":"fails","typeName":"Query"}]`))
	assert.Same(t, failure, err)

	_, err = app.Resolve(t.Context(), jsontext.Value(`[{"fieldName":"typed","typeName":"Query","arguments":{"id":1}}]`))
	var argsErr *ArgumentsError
	require.ErrorAs(t, err, &argsErr)
}

func TestBatchCurrentEventAndContext(t *testing.T) {
	t.Parallel()
	type projectKey struct{}
	app := New()
	router := New()
	BatchResolver(router, AnyType, "createSomething", func(ctx context.Context, events []Event[struct {
		UserID string `json:"user_id"`
	}],
	) ([]string, error) {
		batch, _ := CurrentBatch(ctx)
		project, ok := ctx.Value(projectKey{}).(string)
		require.True(t, ok)
		return []string{batch[0].Identity.Sub, project + "#" + events[1].Arguments.UserID}, nil
	})
	app.Include(router)

	ctx := context.WithValue(t.Context(), projectKey{}, "powertools")
	out, err := app.Resolve(ctx, load(t, "appSyncBatchEvent.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `["192879fc-a240-4bf1-ab5a-d6a00f3063f9","powertools#2"]`, string(out))
}

func TestOnErrorBatchResolver(t *testing.T) {
	t.Parallel()
	for _, raise := range []bool{true, false} {
		t.Run(fmt.Sprint("raise ", raise), func(t *testing.T) {
			t.Parallel()
			app := New()
			OnError(app, func(context.Context, *ValueError) (map[string]string, error) {
				return map[string]string{"message": "error"}, nil
			})
			BatchItemResolver(app, AnyType, "listLocations", func(context.Context, Event[struct{}]) (string, error) {
				return "", &ValueError{Msg: "Error"}
			}, WithRaiseOnError(raise))
			out, err := app.Resolve(t.Context(), jsontext.Value(listLocationsEvent))
			require.NoError(t, err)
			if raise {
				assert.JSONEq(t, `{"message":"error"}`, string(out))
			} else {
				assert.JSONEq(t, `[null,null,null]`, string(out), "item errors never reach the handlers")
			}
		})
	}
}

func TestEmptyBatch(t *testing.T) {
	t.Parallel()
	out, err := New().Resolve(t.Context(), jsontext.Value(` []`))
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, string(out))
}

func TestBatchConcurrencyIsBounded(t *testing.T) {
	t.Parallel()
	const limit = 3
	var running, peak atomic.Int32
	app := New()
	BatchItemResolver(app, AnyType, "f", func(_ context.Context, ev Event[struct {
		N int `json:"n"`
	}],
	) (int, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		return ev.Arguments.N * 10, nil
	}, WithConcurrency(limit))

	items := make([]string, 12)
	for i := range items {
		items[i] = fmt.Sprintf(`{"typeName":"Query","fieldName":"f","arguments":{"n":%d}}`, i)
	}
	got := resolve[[]int](t, app, jsontext.Value("["+strings.Join(items, ",")+"]"))
	assert.Equal(t, []int{0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110}, got, "results keep the order of the batch")
	assert.LessOrEqual(t, peak.Load(), int32(limit))
	assert.Greater(t, peak.Load(), int32(1))
}

func TestBatchRaiseCancelsOtherItems(t *testing.T) {
	t.Parallel()
	failure := errors.New("first")
	var started atomic.Int32
	app := New()
	BatchItemResolver(app, AnyType, "f", func(ctx context.Context, ev Event[idArgs]) (string, error) {
		started.Add(1)
		if ev.Arguments.ID == "fail" {
			return "", failure
		}
		<-ctx.Done() // the failure cancels the others
		return "", context.Cause(ctx)
	}, WithRaiseOnError(true), WithConcurrency(2))

	event := `[{"fieldName":"f","typeName":"Q","arguments":{"id":"slow"}},{"fieldName":"f","typeName":"Q","arguments":{"id":"fail"}},` +
		`{"fieldName":"f","typeName":"Q","arguments":{"id":"never"}},{"fieldName":"f","typeName":"Q","arguments":{"id":"never"}}]`
	_, err := app.Resolve(t.Context(), jsontext.Value(event))
	require.ErrorIs(t, err, failure)
	assert.LessOrEqual(t, started.Load(), int32(3), "no item starts after the failure")
}

func TestBatchConcurrentPanicPropagates(t *testing.T) {
	t.Parallel()
	app := New()
	BatchItemResolver(app, AnyType, "f", func(_ context.Context, ev Event[idArgs]) (string, error) {
		if ev.Arguments.ID == "panic" {
			panic("resolver bug")
		}
		return ev.Arguments.ID, nil
	}, WithConcurrency(2))
	event := `[{"fieldName":"f","typeName":"Q","arguments":{"id":"a"}},{"fieldName":"f","typeName":"Q","arguments":{"id":"panic"}}]`
	assert.PanicsWithValue(t, "resolver bug", func() {
		_, _ = app.Resolve(t.Context(), jsontext.Value(event)) //nolint:errcheck // the property is the panic
	})
}

func TestNilBatchOption(t *testing.T) {
	t.Parallel()
	app := New()
	BatchItemResolver(app, AnyType, "f", func(context.Context, Event[struct{}]) (int, error) { return 1, nil }, nil)
	assert.Equal(t, []int{1}, resolve[[]int](t, app, jsontext.Value(`[{"fieldName":"f"}]`)))
}
