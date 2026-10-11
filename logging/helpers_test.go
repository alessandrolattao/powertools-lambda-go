package logging

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/require"
)

// withWarnings sends the warnings to w instead of standard error.
func withWarnings(w io.Writer) Option {
	return func(c *config) { c.warnings = w }
}

// withColdStart replaces the cold start decision, which is process-wide.
func withColdStart(fn func(context.Context) bool) Option {
	return func(c *config) { c.coldStart = fn }
}

// withFunction replaces the function's name and memory size, which
// aws-lambda-go reads from the environment once, at start up.
func withFunction(name string, memory int) Option {
	return func(c *config) { c.function = &functionInfo{name: name, memory: memory} }
}

// testTime is the time of every record of the tests.
var testTime = time.Date(2026, 10, 10, 12, 0, 0, 123_000_000, time.UTC)

const testTimestamp = "2026-10-10T12:00:00.123Z"

// lockedBuffer is a bytes.Buffer safe for concurrent writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines returns what was written, one string per line.
func (b *lockedBuffer) lines() []string {
	s := strings.TrimSuffix(b.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// records decodes what was written, one map per record.
func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	lines := b.lines()
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), line)
		out = append(out, record)
	}
	return out
}

// fixture is a logger writing to buffers, with a fixed time, a function and
// no cold start, so that its records are known in advance.
type fixture struct {
	logger   *Logger
	out      *lockedBuffer
	warnings *lockedBuffer
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()
	f := &fixture{out: &lockedBuffer{}, warnings: &lockedBuffer{}}
	all := make([]Option, 0, 6+len(opts))
	all = append(all,
		WithService("payment"),
		WithWriter(f.out),
		WithClock(func() time.Time { return testTime }),
		withWarnings(f.warnings),
		withColdStart(func(context.Context) bool { return false }),
		withFunction("test", 128),
	)
	f.logger = New(append(all, opts...)...)
	return f
}

// invoke runs fn as a handler wrapped by the fixture's InjectContext.
func (f *fixture) invoke(ctx context.Context, fn func(ctx context.Context)) {
	h := f.logger.InjectContext(func(ctx context.Context, _ map[string]any) (struct{}, error) {
		fn(ctx)
		return struct{}{}, nil
	})
	_, _ = h(ctx, nil) //nolint:errcheck // the handler never fails
}

// lambdaContext returns ctx inside the invocation requestID.
func lambdaContext(ctx context.Context, requestID string) context.Context {
	return lambdacontext.NewContext(ctx, &lambdacontext.LambdaContext{
		AwsRequestID:       requestID,
		InvokedFunctionArn: "arn:aws:lambda:eu-west-1:123456789012:function:test",
	})
}

// withTraceHeader stores a trace header where aws-lambda-go stores it.
func withTraceHeader(ctx context.Context, header string) context.Context {
	return context.WithValue(ctx, traceHeaderKey, header) //nolint:staticcheck,revive // SA1029, context-keys-type: aws-lambda-go's own key is a plain string
}
