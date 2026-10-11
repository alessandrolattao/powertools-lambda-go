package logging

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
)

type order struct {
	ID string `json:"id"`
}

func TestLogEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	handle := f.logger.InjectContext(func(context.Context, order) (string, error) { return "ok", nil }, WithLogEvent(true))
	_, err := handle(t.Context(), order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, []string{
		`{"level":"INFO","message":{"id":"o-1"},"timestamp":"` + testTimestamp + `","service":"payment"}`,
	}, f.out.lines())
}

func TestLogEventLocatesTheHandler(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	handle := f.logger.InjectContext(handleOrder, WithLogEvent(true))
	_, err := handle(t.Context(), order{ID: "o-1"})
	require.NoError(t, err)
	records := f.out.records(t)
	require.Len(t, records, 1)
	assert.Regexp(t, `^logging\.handleOrder:\d+$`, records[0][KeyLocation])
}

func handleOrder(_ context.Context, o order) (string, error) { return o.ID, nil }

func TestLogRawEvents(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	raw := f.logger.InjectContext(func(context.Context, []byte) (string, error) { return "", nil }, WithLogEvent(true))
	_, err := raw(t.Context(), []byte(`{"a":1}`))
	require.NoError(t, err)
	value := f.logger.InjectContext(func(context.Context, jsontext.Value) (string, error) { return "", nil }, WithLogEvent(true))
	_, err = value(t.Context(), jsontext.Value(`[1,2]`))
	require.NoError(t, err)
	assert.Equal(t, []string{
		`{"level":"INFO","message":{"a":1},"timestamp":"` + testTimestamp + `","service":"payment"}`,
		`{"level":"INFO","message":[1,2],"timestamp":"` + testTimestamp + `","service":"payment"}`,
	}, f.out.lines())
}

func TestLogEventFromEnvironment(t *testing.T) {
	t.Setenv("POWERTOOLS_LOGGER_LOG_EVENT", "true")
	f := newFixture(t, WithoutKeys(KeyLocation))
	_, err := f.logger.InjectContext(handleOrder)(t.Context(), order{ID: "o-1"})
	require.NoError(t, err)
	_, err = f.logger.InjectContext(handleOrder, WithLogEvent(false))(t.Context(), order{ID: "o-2"})
	require.NoError(t, err)
	assert.Len(t, f.out.lines(), 1, "the option wins over the variable")

	t.Setenv("POWERTOOLS_LOGGER_LOG_EVENT", "")
	_, err = f.logger.InjectContext(handleOrder)(t.Context(), order{ID: "o-3"})
	require.NoError(t, err)
	assert.Len(t, f.out.lines(), 1, "events are not logged by default")
}

func TestLogEventBelowTheLevel(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithLevel(slog.LevelWarn))
	_, err := f.logger.InjectContext(handleOrder, WithLogEvent(true))(t.Context(), order{ID: "o-1"})
	require.NoError(t, err)
	assert.Empty(t, f.out.lines())
}

func TestCorrelationPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, event, want string
	}{
		{"API Gateway REST", CorrelationAPIGatewayREST, `{"requestContext":{"requestId":"r-1"}}`, "r-1"},
		{"API Gateway HTTP", CorrelationAPIGatewayHTTP, `{"requestContext":{"requestId":"r-2"}}`, "r-2"},
		{"Function URL", CorrelationLambdaFunctionURL, `{"requestContext":{"requestId":"r-3"}}`, "r-3"},
		{"AppSync authorizer", CorrelationAppSyncAuthorizer, `{"requestContext":{"requestId":"r-4"}}`, "r-4"},
		{"AppSync resolver", CorrelationAppSyncResolver, `{"request":{"headers":{"x-amzn-trace-id":"t-1"}}}`, "t-1"},
		{"ALB", CorrelationApplicationLoadBalancer, `{"headers":{"x-amzn-trace-id":"t-2"}}`, "t-2"},
		{"VPC Lattice", CorrelationVPCLattice, `{"headers":{"x-amzn-trace-id":"t-3"}}`, "t-3"},
		{"EventBridge", CorrelationEventBridge, `{"id":"e-1"}`, "e-1"},
		{"S3 Object Lambda", CorrelationS3ObjectLambda, `{"xAmzRequestId":"s-1"}`, "s-1"},
		{"number", "n", `{"n":42}`, "42"},
		{"missing", "nope", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, WithoutKeys(KeyLocation))
			var seen string
			handle := f.logger.InjectContext(func(ctx context.Context, _ jsontext.Value) (string, error) {
				seen = CorrelationID(ctx)
				f.logger.InfoContext(ctx, "hello")
				return "", nil
			}, WithCorrelationIDPath(tc.path))
			_, err := handle(t.Context(), jsontext.Value(tc.event))
			require.NoError(t, err)
			assert.Equal(t, tc.want, seen)
			records := f.out.records(t)
			require.Len(t, records, 1)
			if tc.want == "" {
				assert.NotContains(t, records[0], KeyCorrelationID)
				return
			}
			assert.Equal(t, tc.want, fmt.Sprint(records[0][KeyCorrelationID]))
		})
	}
}

func TestCorrelationIDFromAnEventStruct(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	var seen string
	handle := f.logger.InjectContext(func(ctx context.Context, _ events.APIGatewayV2HTTPRequest) (string, error) {
		seen = CorrelationID(ctx)
		return "", nil
	}, WithCorrelationIDPath(CorrelationAPIGatewayHTTP))
	_, err := handle(t.Context(), events.APIGatewayV2HTTPRequest{
		RequestContext: events.APIGatewayV2HTTPRequestContext{RequestID: "r-9"},
	})
	require.NoError(t, err)
	assert.Equal(t, "r-9", seen)
}

func TestCorrelationPathErrorsFailTheInvocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	called := false
	next := func(context.Context, jsontext.Value) (string, error) {
		called = true
		return "", nil
	}
	_, err := f.logger.InjectContext(next, WithCorrelationIDPath("a[?"))(t.Context(), jsontext.Value(`{}`))
	require.Error(t, err)
	_, err = f.logger.InjectContext(next, WithCorrelationIDPath("powertools_json(a)"))(t.Context(), jsontext.Value(`{"a":"{"}`))
	require.Error(t, err)
	assert.False(t, called)
}

func TestClearState(t *testing.T) {
	t.Parallel()
	outer := newFixture(t, WithoutKeys(KeyLocation))
	inner := newFixture(t, WithoutKeys(KeyLocation))
	run := func(opts ...InjectOption) {
		handle := inner.logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
			inner.logger.InfoContext(ctx, "hello")
			return "", nil
		}, opts...)
		wrapped := outer.logger.InjectContext(func(ctx context.Context, event string) (string, error) {
			AppendKeys(ctx, "outer", true)
			return handle(WithKeys(ctx, "scoped", true), event)
		})
		_, err := wrapped(t.Context(), "")
		require.NoError(t, err)
	}
	run()
	run(WithClearState(true))
	assert.Equal(t, []string{head + `,"outer":true,"scoped":true}`, head + `}`}, inner.out.lines())
}

func TestReturnedErrorsAreNotLogged(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	errDeclined := errors.New("declined")
	_, err := f.logger.InjectContext(func(context.Context, string) (string, error) { return "", errDeclined })(t.Context(), "")
	require.ErrorIs(t, err, errDeclined)
	assert.Empty(t, f.out.lines())
}

func TestUncaughtPanic(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithLogUncaughtPanics(true), WithBuffer())
	handle := f.logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
		f.logger.DebugContext(ctx, "before the panic")
		count(nil, "o-1")
		return "", nil
	})
	assert.PanicsWithError(t, "assignment to entry in nil map", func() { _, _ = handle(t.Context(), "") }) //nolint:errcheck // it panics

	records := f.out.records(t)
	require.Len(t, records, 2, "the buffered record, then the panic")
	assert.Equal(t, "before the panic", records[0][KeyMessage])
	panicRecord := records[1]
	assert.Equal(t, "ERROR", panicRecord[KeyLevel])
	assert.Equal(t, "assignment to entry in nil map", panicRecord[KeyMessage])
	assert.Equal(t, "assignment to entry in nil map", panicRecord[KeyException])
	assert.Equal(t, "plainError", panicRecord[KeyExceptionName])
	assert.Regexp(t, `^logging\.count:\d+$`, panicRecord[KeyLocation], "located where it panicked")
	st, ok := panicRecord[KeyStackTrace].(map[string]any)
	require.True(t, ok)
	frames, ok := st["frames"].([]any)
	require.True(t, ok)
	first, ok := frames[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "github.com/alessandrolattao/powertools-lambda-go/logging.count", first["function"])
	second, ok := frames[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "github.com/alessandrolattao/powertools-lambda-go/logging.TestUncaughtPanic.func1", second["function"])
}

func TestUncaughtPanicValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value     any
		exception string
		name      string
	}{
		{"boom", "boom", "panic"},
		{42, "42", "panic"},
		{&os.PathError{Op: "open", Path: "/x", Err: os.ErrNotExist}, "open /x: file does not exist", "PathError"},
		{errors.New("plain"), "plain", "panic"},
	}
	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, WithLogUncaughtPanics(true))
			handle := f.logger.InjectContext(func(context.Context, string) (string, error) { panic(tc.value) })
			assert.Panics(t, func() { _, _ = handle(t.Context(), "") }) //nolint:errcheck // it panics
			records := f.out.records(t)
			require.Len(t, records, 1)
			assert.Equal(t, tc.exception, records[0][KeyException])
			assert.Equal(t, tc.name, records[0][KeyExceptionName])
		})
	}
}

func TestPanicsAreNotLoggedByDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	handle := f.logger.InjectContext(func(context.Context, string) (string, error) { panic("boom") })
	assert.PanicsWithValue(t, "boom", func() { _, _ = handle(t.Context(), "") }) //nolint:errcheck // it panics
	assert.Empty(t, f.out.lines())
}

func TestColdStart(t *testing.T) {
	t.Setenv("AWS_LAMBDA_INITIALIZATION_TYPE", "on-demand")
	coldstart.Reset()
	t.Cleanup(coldstart.Reset)
	out := &lockedBuffer{}
	logger := New(WithWriter(out), withFunction("test", 128))
	handle := logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
		logger.InfoContext(ctx, "hello")
		logger.InfoContext(ctx, "again")
		return "", nil
	})
	for _, id := range []string{"req-1", "req-2"} {
		_, err := handle(lambdaContext(t.Context(), id), "")
		require.NoError(t, err)
	}
	records := out.records(t)
	cold := make([]any, 0, len(records))
	for _, record := range records {
		cold = append(cold, record[KeyColdStart])
	}
	assert.Equal(t, []any{true, true, false, false}, cold)
}

func TestColdStartWithoutInjectContext(t *testing.T) {
	t.Setenv("AWS_LAMBDA_INITIALIZATION_TYPE", "on-demand")
	coldstart.Reset()
	t.Cleanup(coldstart.Reset)
	out := &lockedBuffer{}
	logger := New(WithWriter(out))
	logger.InfoContext(lambdaContext(t.Context(), "req-1"), "hello")
	logger.InfoContext(lambdaContext(t.Context(), "req-1"), "hello")
	logger.InfoContext(lambdaContext(t.Context(), "req-2"), "hello")
	logger.InfoContext(t.Context(), "outside Lambda")
	records := out.records(t)
	cold := make([]any, 0, len(records))
	for _, record := range records {
		cold = append(cold, record[KeyColdStart])
	}
	assert.Equal(t, []any{true, true, false, nil}, cold)
}

func TestNestedInjectContextOfOneLogger(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation), WithBuffer())
	inner := f.logger.InjectContext(func(ctx context.Context, _ string) (string, error) {
		f.logger.DebugContext(ctx, "buffered")
		return "", nil
	})
	outer := f.logger.InjectContext(func(ctx context.Context, event string) (string, error) {
		_, err := inner(ctx, event)
		require.NoError(t, f.logger.FlushBuffer(ctx), "the inner invocation does not end the outer one")
		return "", err
	})
	_, err := outer(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, f.out.lines(), 1)
}

func TestEventsAreEncodedLikeJSON(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation))
	_, err := f.logger.InjectContext(handleOrder, WithLogEvent(true))(t.Context(), order{ID: "o-1"})
	require.NoError(t, err)
	want, err := json.Marshal(order{ID: "o-1"})
	require.NoError(t, err)
	assert.Contains(t, f.out.String(), `"message":`+string(want))
}

// count increments a counter of orders; a nil map makes it panic.
func count(orders map[string]int, id string) {
	orders[id]++
}
