package appsyncevents

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
// lambda.Start as it is, its JSON reaches AppSync untouched, and an
// unauthorized operation fails with the errorType AppSync Events expects.
func TestLambdaRuntime(t *testing.T) {
	app := New()
	OnPublish(app, DefaultPath, func(_ context.Context, payload jsontext.Value) (jsontext.Value, error) {
		return payload, nil
	})
	OnPublish(app, "/secret/*", func(context.Context, data) (data, error) {
		return data{}, &UnauthorizedError{Message: "You can only publish to /default/foo"}
	})
	app.OnSubscribe("/*", func(context.Context) error {
		return &UnauthorizedError{Message: "You are not authorized to subscribe to this channel"}
	})
	app.OnSubscribe("/default/*", func(context.Context) error { return nil })

	rt, address := newFakeRuntime(
		publish(t, "/default/channel", `[{"id":"1","payload":{"big":9007199254740993,"html":"<b>"}}]`),
		publish(t, "/secret/channel", oneEvent),
		subscribe(t, "/default/channel"),
		subscribe(t, "/other/channel"),
	)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", address)
	answers := rt.run(t, app.Resolve)

	assert.Equal(t, runtimeAnswer{body: `{"events":[{"id":"1","payload":{"big":9007199254740993,"html":"<b>"}}]}`}, answers[0])
	assert.True(t, answers[1].failed)
	assert.JSONEq(t, `{"errorType":"UnauthorizedException","errorMessage":"You can only publish to /default/foo"}`, answers[1].body)
	assert.Equal(t, runtimeAnswer{body: `null`}, answers[2])
	assert.True(t, answers[3].failed)
	assert.JSONEq(t, `{"errorType":"UnauthorizedException","errorMessage":"You are not authorized to subscribe to this channel"}`,
		answers[3].body)
}
