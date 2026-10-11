package integration

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
)

// loggingFunctionEnv makes the test binary run as a Lambda function instead
// of running the tests: TestLoggingInTheLambdaRuntime starts it so, to run
// the logger inside aws-lambda-go's real invoke loop.
const loggingFunctionEnv = "POWERTOOLS_INTEGRATION_LOGGING_FUNCTION"

type loggingEvent struct {
	ID   string `json:"id"`
	Fail bool   `json:"fail"`
}

// init runs the function before TestMain, when the test binary was started
// as one, and never returns: lambda.Start serves invocations until the test
// kills the process.
func init() {
	if os.Getenv(loggingFunctionEnv) != "1" {
		return
	}
	logger := logging.New(logging.WithService("integration"), logging.WithBuffer())
	lambda.Start(logger.InjectContext(func(ctx context.Context, event loggingEvent) (string, error) {
		logging.AppendKeys(ctx, "invocation", event.ID)
		logger.DebugContext(ctx, "buffered")
		time.Sleep(5 * time.Millisecond) // let the invocations overlap
		logger.InfoContext(ctx, "handled")
		if event.Fail {
			logger.ErrorContext(ctx, "failed")
		}
		return event.ID, nil
	}, logging.WithCorrelationIDPath("id")))
}

// runtimeAPI is a fake Lambda Runtime API serving a fixed list of events.
type runtimeAPI struct {
	events    chan loggingEvent
	mu        sync.Mutex
	responses map[string]string
	done      chan struct{}
	total     int
}

func newRuntimeAPI(events []loggingEvent) *runtimeAPI {
	api := &runtimeAPI{
		events:    make(chan loggingEvent, len(events)),
		responses: map[string]string{},
		done:      make(chan struct{}),
		total:     len(events),
	}
	for _, e := range events {
		api.events <- e
	}
	return api
}

func (api *runtimeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /2018-06-01/runtime/invocation/next", func(w http.ResponseWriter, r *http.Request) {
		select {
		case e := <-api.events:
			w.Header().Set("Lambda-Runtime-Aws-Request-Id", "req-"+e.ID)
			w.Header().Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10))
			w.Header().Set("Lambda-Runtime-Invoked-Function-Arn", "arn:aws:lambda:eu-west-1:123456789012:function:integration")
			w.Header().Set("Lambda-Runtime-Trace-Id", traceHeader(e.ID))
			payload, err := json.Marshal(e)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(payload) //nolint:errcheck // the function sees a failed read
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/{id}/response", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // an empty body fails the assertions
		api.record(r.PathValue("id"), string(body))
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/{id}/error", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // an empty body fails the assertions
		api.record(r.PathValue("id"), "error: "+string(body))
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

func (api *runtimeAPI) record(id, response string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.responses[id] = response
	if len(api.responses) == api.total {
		close(api.done)
	}
}

func traceID(id string) string {
	return fmt.Sprintf("1-00000000-%024s", id)
}

func traceHeader(id string) string {
	return "Root=" + traceID(id) + ";Parent=53995c3f42cd8ad8;Sampled=1"
}

// TestLoggingInTheLambdaRuntime runs a function in aws-lambda-go's invoke
// loop with four concurrent invocations, as Lambda Managed Instances do, and
// checks that every record carries its own invocation's request id, trace
// id (from the context: aws-lambda-go does not set _X_AMZN_TRACE_ID with
// concurrency), correlation id and keys, and that only failing invocations
// flush their buffer.
func TestLoggingInTheLambdaRuntime(t *testing.T) {
	t.Parallel()
	const invocations = 40
	events := make([]loggingEvent, invocations)
	for i := range events {
		events[i] = loggingEvent{ID: strconv.Itoa(i), Fail: i%4 == 0}
	}
	api := newRuntimeAPI(events)
	server := httptest.NewServer(api.handler())
	defer server.Close()

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0]) //nolint:gosec // G204: the test binary itself, run as the function
	cmd.Env = append(withoutTraceID(os.Environ()),
		loggingFunctionEnv+"=1",
		"AWS_LAMBDA_RUNTIME_API="+strings.TrimPrefix(server.URL, "http://"),
		"AWS_LAMBDA_MAX_CONCURRENCY=4",
		"AWS_LAMBDA_FUNCTION_NAME=integration",
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE=256",
		"AWS_LAMBDA_INITIALIZATION_TYPE=on-demand",
	)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Start())

	select {
	case <-api.done:
	case <-ctx.Done():
		t.Fatalf("the function did not answer every invocation; stderr:\n%s", stderr.String())
	}
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait() //nolint:errcheck // killed on purpose
	server.CloseClientConnections()

	for _, e := range events {
		assert.JSONEq(t, strconv.Quote(e.ID), api.responses["req-"+e.ID], e.ID)
	}
	checkRuntimeRecords(t, stdout.String(), events)
}

func checkRuntimeRecords(t *testing.T, stdout string, events []loggingEvent) {
	t.Helper()
	messages := map[string][]string{}
	coldInvocations := map[string]bool{}
	for line := range strings.Lines(stdout) {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), line)
		id, ok := record["invocation"].(string)
		require.True(t, ok, "a record without its invocation: %s", line)
		assert.Equal(t, "req-"+id, record[logging.KeyFunctionRequestID], line)
		assert.Equal(t, id, record[logging.KeyCorrelationID], line)
		assert.Equal(t, traceID(id), record[logging.KeyXRayTraceID], line)
		assert.Equal(t, "integration", record[logging.KeyFunctionName], line)
		assert.InDelta(t, 256, record[logging.KeyFunctionMemorySize], 0, line)
		if cold, _ := record[logging.KeyColdStart].(bool); cold { //nolint:errcheck // absent means not cold
			coldInvocations[id] = true
		}
		messages[id] = append(messages[id], fmt.Sprint(record[logging.KeyMessage]))
	}
	assert.Len(t, coldInvocations, 1, "one cold invocation")
	for _, e := range events {
		want := []string{"handled"}
		if e.Fail {
			want = []string{"handled", "buffered", "failed"}
		}
		assert.Equal(t, want, messages[e.ID], "invocation %s", e.ID)
	}
}

// withoutTraceID drops _X_AMZN_TRACE_ID from an environment, so that the
// trace ids can only come from the context.
func withoutTraceID(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "_X_AMZN_TRACE_ID=") {
			out = append(out, kv)
		}
	}
	return out
}
