package appsync

import (
	"context"
	"encoding/json/jsontext"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambda/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRuntime is a Lambda Runtime API that hands out a list of invocations
// and records what the function answers, so that a handler runs through
// aws-lambda-go's real runtime loop: its JSON decoding and encoding, and
// the errorType it reports for a returned error.
type fakeRuntime struct {
	mu      sync.Mutex
	events  []jsontext.Value
	next    int
	answers chan runtimeAnswer
}

type runtimeAnswer struct {
	failed bool
	body   string
}

func (rt *fakeRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/invocation/next"):
		rt.mu.Lock()
		if rt.next == len(rt.events) {
			rt.mu.Unlock()
			// Nothing left: hold the loop here for the rest of the test
			// binary, since aws-lambda-go exits the process when the
			// Runtime API fails.
			<-r.Context().Done()
			return
		}
		event, id := rt.events[rt.next], "req-"+strconv.Itoa(rt.next)
		rt.next++
		rt.mu.Unlock()
		w.Header().Set("Lambda-Runtime-Aws-Request-Id", id)
		w.Header().Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10))
		_, _ = w.Write(event) //nolint:errcheck // a broken response fails the runtime loop, and the test with it
	case r.Method == http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rt.answers <- runtimeAnswer{failed: strings.HasSuffix(r.URL.Path, "/error"), body: string(body)}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

// newFakeRuntime serves events and returns the address to set as
// AWS_LAMBDA_RUNTIME_API. The server is never closed: the runtime loop
// keeps waiting on it.
func newFakeRuntime(events ...jsontext.Value) (rt *fakeRuntime, address string) {
	rt = &fakeRuntime{events: events, answers: make(chan runtimeAnswer, len(events))}
	srv := httptest.NewServer(rt)
	return rt, strings.TrimPrefix(srv.URL, "http://")
}

// run starts handler with lambda.Start and returns its answers, in order.
func (rt *fakeRuntime) run(t *testing.T, handler any) []runtimeAnswer {
	t.Helper()
	go lambda.Start(handler)
	answers := make([]runtimeAnswer, 0, len(rt.events))
	for range rt.events {
		select {
		case a := <-rt.answers:
			answers = append(answers, a)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "the function did not answer")
		}
	}
	return answers
}

// TestLambdaRuntime runs the router as a Lambda function: Resolve goes to
// lambda.Start as it is, its JSON reaches AppSync untouched, and a failure
// carries the errorType AppSync shows in the GraphQL error.
func TestLambdaRuntime(t *testing.T) {
	app := New()
	type todo struct {
		ID   string         `json:"id"`
		Big  jsontext.Value `json:"big"`
		HTML string         `json:"html"`
	}
	Resolver(app, "Query", "getTodo", func(_ context.Context, args idArgs) (todo, error) {
		return todo{ID: args.ID, Big: jsontext.Value("9007199254740993"), HTML: "<b>"}, nil
	})
	Resolver(app, "Query", "custom", func(context.Context, struct{}) (string, error) {
		return "", messages.InvokeResponse_Error{Type: "NotFound", Message: "todo not found"}
	})
	Resolver(app, "Query", "mine", func(context.Context, struct{}) (string, error) {
		return "", &ValueError{Msg: "bad value"}
	})
	BatchItemResolver(app, "Query", "batch", func(_ context.Context, ev Event[idArgs]) (string, error) {
		if ev.Arguments.ID == "" {
			return "", &ValueError{Msg: "no id"}
		}
		return ev.Arguments.ID, nil
	})

	rt, address := newFakeRuntime(
		jsontext.Value(`{"info":{"parentTypeName":"Query","fieldName":"getTodo"},"arguments":{"id":"t1"}}`),
		jsontext.Value(`{"info":{"parentTypeName":"Query","fieldName":"missing"}}`),
		jsontext.Value(`{"info":{"parentTypeName":"Query","fieldName":"custom"}}`),
		jsontext.Value(`{"info":{"parentTypeName":"Query","fieldName":"mine"}}`),
		jsontext.Value(`[{"info":{"parentTypeName":"Query","fieldName":"batch"},"arguments":{"id":"a"}},`+
			`{"info":{"parentTypeName":"Query","fieldName":"batch"},"arguments":{}}]`),
	)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", address)
	answers := rt.run(t, app.Resolve)

	assert.Equal(t, runtimeAnswer{body: `{"id":"t1","big":9007199254740993,"html":"<b>"}`}, answers[0])
	assert.True(t, answers[1].failed)
	assert.JSONEq(t, `{"errorType":"ResolverNotFoundError","errorMessage":"No resolver found for 'Query.missing'"}`, answers[1].body)
	assert.JSONEq(t, `{"errorType":"NotFound","errorMessage":"todo not found"}`, answers[2].body)
	assert.JSONEq(t, `{"errorType":"ValueError","errorMessage":"bad value"}`, answers[3].body)
	assert.Equal(t, runtimeAnswer{body: `["a",null]`}, answers[4])
}
