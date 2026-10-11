package logging

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

const (
	testARN     = "arn:aws:lambda:eu-west-1:123456789012:function:test"
	testTraceID = "1-5759e988-bd862e3fe1be46a994272793"
	testHeader  = "Root=" + testTraceID + ";Parent=53995c3f42cd8ad8;Sampled=1"
	// head is how every record of the golden tests begins.
	head = `{"level":"INFO","message":"hello","timestamp":"` + testTimestamp + `","service":"payment"`
	// lambdaKeys are the keys of the Lambda context req-1.
	lambdaKeys = `"cold_start":false,"function_name":"test","function_memory_size":128,` +
		`"function_arn":"` + testARN + `","function_request_id":"req-1"`
)

// badJSON cannot be encoded as JSON, and has a text of its own.
type badJSON struct{}

func (badJSON) MarshalJSON() ([]byte, error) { return nil, errors.New("no JSON for me") }
func (badJSON) String() string               { return "bad" }

// lazy is a slog.LogValuer.
type lazy struct{}

func (lazy) LogValue() slog.Value { return slog.StringValue("resolved") }

// tracedError remembers where it was created.
type tracedError struct{ pcs []uintptr }

func newTracedError() *tracedError {
	pcs := make([]uintptr, 8)
	return &tracedError{pcs: pcs[:runtime.Callers(1, pcs)]}
}

func (*tracedError) Error() string        { return "traced" }
func (e *tracedError) Callers() []uintptr { return e.pcs }

func info(ctx context.Context, l *Logger) { l.InfoContext(ctx, "hello") }

func TestGoldenRecords(t *testing.T) {
	t.Parallel()
	spanID, err := trace.SpanIDFromHex("53995c3f42cd8ad8")
	require.NoError(t, err)
	traceID, err := trace.TraceIDFromHex("5759e988bd862e3fe1be46a994272793")
	require.NoError(t, err)
	cases := []struct {
		name string
		opts []Option
		ctx  func(context.Context) context.Context
		log  func(context.Context, *Logger)
		want string
	}{
		{
			name: "standard keys",
			want: head + `}`,
		},
		{
			name: "lambda context",
			ctx:  func(ctx context.Context) context.Context { return lambdaContext(ctx, "req-1") },
			want: head + `,` + lambdaKeys + `}`,
		},
		{
			name: "cold start",
			opts: []Option{withColdStart(func(context.Context) bool { return true }), withFunction("", 0)},
			ctx:  func(ctx context.Context) context.Context { return lambdaContext(ctx, "req-1") },
			want: head + `,"cold_start":true,"function_arn":"` + testARN + `","function_request_id":"req-1"}`,
		},
		{
			name: "sampling rate",
			opts: []Option{WithSamplingRate(0.1), WithRandom(func() float64 { return 0.5 })},
			want: head + `,"sampling_rate":0.1}`,
		},
		{
			name: "xray trace id from the span",
			ctx: func(ctx context.Context) context.Context {
				ctx = withTraceHeader(ctx, "Root=1-00000000-000000000000000000000000")
				return trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))
			},
			want: head + `,"xray_trace_id":"` + testTraceID + `"}`,
		},
		{
			name: "xray trace id from the header",
			ctx: func(ctx context.Context) context.Context {
				return withTraceHeader(lambdaContext(ctx, "req-1"), testHeader)
			},
			want: head + `,` + lambdaKeys + `,"xray_trace_id":"` + testTraceID + `"}`,
		},
		{
			name: "exception",
			log: func(ctx context.Context, l *Logger) {
				l.ErrorContext(ctx, "hello", "err", fmt.Errorf("charge: %w", &os.PathError{Op: "open", Path: "/x", Err: os.ErrNotExist}))
			},
			want: `{"level":"ERROR","message":"hello","timestamp":"` + testTimestamp + `","service":"payment",` +
				`"exception":"charge: open /x: file does not exist","exception_name":"PathError"}`,
		},
		{
			name: "exception of a plain error",
			log:  func(ctx context.Context, l *Logger) { l.InfoContext(ctx, "hello", "err", errors.New("boom")) },
			want: head + `,"exception":"boom","exception_name":"error"}`,
		},
		{
			name: "exception keys set by the record win",
			log: func(ctx context.Context, l *Logger) {
				l.InfoContext(ctx, "hello", "exception", "custom", "err", errors.New("boom"))
			},
			want: head + `,"exception":"custom","exception_name":"error"}`,
		},
		{
			name: "a nil error is no exception",
			log:  func(ctx context.Context, l *Logger) { l.InfoContext(ctx, "hello", "err", error(nil)) },
			want: head + `}`,
		},
		{
			name: "an error in a group is its message",
			log: func(ctx context.Context, l *Logger) {
				l.WithGroup("g").InfoContext(ctx, "hello", "err", errors.New("boom"))
			},
			want: head + `,"g":{"err":"boom"}}`,
		},
		{
			name: "nil removes a key",
			log:  func(ctx context.Context, l *Logger) { l.With("k", 1).InfoContext(ctx, "hello", "k", nil) },
			want: head + `}`,
		},
		{
			name: "a later value wins in place",
			log:  func(ctx context.Context, l *Logger) { l.With("k", 1, "j", 2).InfoContext(ctx, "hello", "k", 3) },
			want: head + `,"k":3,"j":2}`,
		},
		{
			name: "Powertools keys can be overridden",
			log:  func(ctx context.Context, l *Logger) { l.InfoContext(ctx, "hello", "service", "other") },
			want: `{"level":"INFO","message":"hello","timestamp":"` + testTimestamp + `","service":"other"}`,
		},
		{
			name: "standard keys cannot be overridden",
			log: func(ctx context.Context, l *Logger) {
				l.InfoContext(ctx, "hello", "level", "x", "message", "y", "timestamp", "z", "location", "w")
			},
			want: head + `}`,
		},
		{
			name: "groups",
			log: func(ctx context.Context, l *Logger) {
				l.With("a", 1).WithGroup("g").With("b", 2).WithGroup("h").InfoContext(ctx, "hello", "c", 3)
			},
			want: head + `,"a":1,"g":{"b":2,"h":{"c":3}}}`,
		},
		{
			name: "empty groups are left out",
			log:  func(ctx context.Context, l *Logger) { l.WithGroup("g").InfoContext(ctx, "hello") },
			want: head + `}`,
		},
		{
			name: "inline groups",
			log: func(ctx context.Context, l *Logger) {
				l.With(slog.Group("", "a", 1), slog.Group("g", slog.Group("", "b", 2))).InfoContext(ctx, "hello")
			},
			want: head + `,"a":1,"g":{"b":2}}`,
		},
		{
			name: "values",
			log: func(ctx context.Context, l *Logger) {
				l.InfoContext(ctx, "hello",
					"s", "str", "i", -1, "u", uint64(2), "f", 1.5, "b", true, "d", time.Second,
					"t", time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), "nan", math.NaN(),
					"obj", struct {
						A int `json:"a"`
					}{A: 1},
					"map", map[string]int{"b": 2, "a": 1}, "bytes", []byte("hi"), "bad", badJSON{},
					"lazy", lazy{}, "e", errors.New("x"), "utf8", "caf\xe9",
				)
			},
			want: head + `,"s":"str","i":-1,"u":2,"f":1.5,"b":true,"d":1000000000,"t":"2026-01-02T03:04:05.000000006Z",` +
				`"nan":"NaN","obj":{"a":1},"map":{"a":1,"b":2},"bytes":"aGk=","bad":"bad","lazy":"resolved","e":"x",` +
				`"utf8":"caf` + "�" + `"}`,
		},
		{
			name: "trace and fatal levels",
			opts: []Option{WithLevel(LevelTrace)},
			log: func(ctx context.Context, l *Logger) {
				l.Log(ctx, LevelTrace, "hello")
				l.Log(ctx, LevelFatal+1, "hello")
			},
			want: `{"level":"TRACE","message":"hello","timestamp":"` + testTimestamp + `","service":"payment"}` + "\n" +
				`{"level":"FATAL","message":"hello","timestamp":"` + testTimestamp + `","service":"payment"}`,
		},
		{
			name: "Python's level names and timestamp",
			opts: []Option{WithLevelNames(PythonLevelName), WithTimestampFormat(TimestampPython)},
			log: func(ctx context.Context, l *Logger) {
				l.WarnContext(ctx, "hello")
				l.Log(ctx, LevelCritical, "hello")
			},
			want: `{"level":"WARNING","message":"hello","timestamp":"2026-10-10 12:00:00,123+0000","service":"payment"}` + "\n" +
				`{"level":"CRITICAL","message":"hello","timestamp":"2026-10-10 12:00:00,123+0000","service":"payment"}`,
		},
		{
			name: "UTC",
			opts: []Option{WithClock(func() time.Time { return testTime.In(time.FixedZone("CEST", 2*3600)) })},
			want: head + `}`,
		},
		{
			name: "local time",
			opts: []Option{WithUTC(false), WithClock(func() time.Time { return testTime.In(time.FixedZone("CEST", 2*3600)) })},
			want: `{"level":"INFO","message":"hello","timestamp":"` + testTime.Local().Format(TimestampRFC3339Milli) + `","service":"payment"}`,
		},
		{
			name: "record order",
			opts: []Option{WithRecordOrder(KeyService, KeyCorrelationID, KeyMessage)},
			want: `{"service":"payment","message":"hello","level":"INFO","timestamp":"` + testTimestamp + `"}`,
		},
		{
			name: "renamed keys",
			opts: []Option{WithKeyName(KeyMessage, "msg"), WithKeyName(KeyFunctionRequestID, "requestId")},
			ctx:  func(ctx context.Context) context.Context { return lambdaContext(ctx, "req-1") },
			log:  func(ctx context.Context, l *Logger) { l.InfoContext(ctx, "hello", "msg", "not the message") },
			want: `{"level":"INFO","msg":"hello","timestamp":"` + testTimestamp + `","service":"payment",` +
				`"cold_start":false,"function_name":"test","function_memory_size":128,"function_arn":"` + testARN + `","requestId":"req-1"}`,
		},
		{
			name: "dropped keys",
			opts: []Option{WithoutKeys(KeyTimestamp, KeyService, KeyXRayTraceID, "secret")},
			ctx:  func(ctx context.Context) context.Context { return withTraceHeader(ctx, testHeader) },
			log:  func(ctx context.Context, l *Logger) { l.InfoContext(ctx, "hello", "secret", "s3cr3t") },
			want: `{"level":"INFO","message":"hello"}`,
		},
		{
			name: "ReplaceAttr",
			opts: []Option{WithReplaceAttr(func(groups []string, a slog.Attr) slog.Attr {
				switch {
				case a.Key == KeyService:
					return slog.String(a.Key, strings.ToUpper(a.Value.String()))
				case a.Key == KeyLevel:
					level, _ := a.Value.Any().(slog.Level) //nolint:errcheck // the level key holds a slog.Level
					return slog.String("severity", level.String())
				case a.Key == "secret":
					return slog.Attr{}
				case len(groups) == 1 && groups[0] == "g":
					return slog.Int(a.Key, 7)
				}
				return a
			})},
			log: func(ctx context.Context, l *Logger) {
				l.InfoContext(ctx, "hello", "secret", "x", "g", map[string]int{"y": 1})
			},
			want: `{"severity":"INFO","message":"hello","timestamp":"` + testTimestamp + `","service":"PAYMENT","g":{"y":1}}`,
		},
		{
			name: "ReplaceAttr in groups",
			opts: []Option{WithReplaceAttr(func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 2 && groups[0] == "g" && groups[1] == "h" {
					return slog.Int(a.Key, 7)
				}
				if len(groups) > 0 && a.Key == "drop" {
					return slog.Attr{}
				}
				return a
			})},
			log: func(ctx context.Context, l *Logger) {
				l.WithGroup("g").With("drop", 1, slog.Group("h", "x", 1), slog.Group("e", "drop", 2)).InfoContext(ctx, "hello")
			},
			want: head + `,"g":{"h":{"x":7}}}`,
		},
		{
			name: "indent",
			opts: []Option{WithIndent("  ")},
			want: "{\n  \"level\": \"INFO\",\n  \"message\": \"hello\",\n  \"timestamp\": \"" + testTimestamp + "\",\n  \"service\": \"payment\"\n}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, append([]Option{WithoutKeys(KeyLocation)}, tc.opts...)...)
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}
			log := tc.log
			if log == nil {
				log = info
			}
			log(ctx, f.logger)
			assert.Equal(t, tc.want+"\n", f.out.String())
			assert.Empty(t, f.warnings.String())
		})
	}
}

func TestLocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, _, line, _ := runtime.Caller(0)
	f.logger.InfoContext(t.Context(), "hello")
	records := f.out.records(t)
	require.Len(t, records, 1)
	assert.Equal(t, "logging.TestLocation:"+strconv.Itoa(line+1), records[0][KeyLocation])

	require.NoError(t, f.logger.Handler().Handle(t.Context(), slog.NewRecord(testTime, slog.LevelInfo, "no caller", 0)))
	assert.NotContains(t, f.out.lines()[1], KeyLocation)
}

func TestStackTrace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.logger.ErrorContext(t.Context(), "failed", "err", fmt.Errorf("wrapped: %w", newTracedError()))
	records := f.out.records(t)
	require.Len(t, records, 1)
	assert.Equal(t, "tracedError", records[0][KeyExceptionName])
	st, ok := records[0][KeyStackTrace].(map[string]any)
	require.True(t, ok, "stack_trace is an object")
	assert.Equal(t, "tracedError", st["type"])
	assert.Equal(t, "wrapped: traced", st["value"])
	assert.Equal(t, "github.com/alessandrolattao/powertools-lambda-go/logging", st["module"])
	frames, ok := st["frames"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, frames)
	first, ok := frames[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "github.com/alessandrolattao/powertools-lambda-go/logging.newTracedError", first["function"])
	assert.Contains(t, first["file"], "handler_test.go")

	f = newFixture(t, WithStackTrace(false))
	f.logger.ErrorContext(t.Context(), "failed", "err", newTracedError())
	assert.NotContains(t, f.out.String(), KeyStackTrace)
}

func TestDevModeIndents(t *testing.T) {
	t.Setenv("POWERTOOLS_DEV", "true")
	f := newFixture(t, WithoutKeys(KeyLocation))
	info(t.Context(), f.logger)
	assert.Equal(t, []string{
		"{", `    "level": "INFO",`, `    "message": "hello",`, `    "timestamp": "` + testTimestamp + `",`, `    "service": "payment"`, "}",
	}, f.out.lines())
}

func TestXRayTraceIDFromEnvironment(t *testing.T) {
	t.Setenv("_X_AMZN_TRACE_ID", testHeader)
	f := newFixture(t, WithoutKeys(KeyLocation))
	info(t.Context(), f.logger)
	assert.Equal(t, []string{head + `,"xray_trace_id":"` + testTraceID + `"}`}, f.out.lines())
}

func TestServiceFromEnvironment(t *testing.T) {
	t.Setenv("POWERTOOLS_SERVICE_NAME", "orders")
	assert.Equal(t, "orders", NewHandler().Service())
	assert.Equal(t, "explicit", New(WithService("explicit")).Service())

	t.Setenv("POWERTOOLS_SERVICE_NAME", "")
	assert.Equal(t, "service_undefined", New().Service())
}

func TestTraceRoot(t *testing.T) {
	t.Parallel()
	cases := []struct{ header, want string }{
		{testHeader, testTraceID},
		{"Parent=1;Root=1-2-3;Sampled=0", "1-2-3"},
		{" root = 1-2-3 ", "1-2-3"},
		{"Parent=53995c3f42cd8ad8;Sampled=1", ""},
		{"", ""},
		{"garbage", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, traceRoot(tc.header), tc.header)
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestHandleReportsWriteErrors(t *testing.T) {
	t.Parallel()
	h := NewHandler(WithWriter(failingWriter{}))
	err := h.Handle(t.Context(), slog.NewRecord(testTime, slog.LevelInfo, "hello", 0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk full")
}

func TestJSONOptions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithoutKeys(KeyLocation), WithJSONOptions(json2Marshalers()))
	f.logger.InfoContext(t.Context(), "hello", "d", celsius(21.5))
	assert.Equal(t, []string{head + `,"d":"21.5°C"}`}, f.out.lines())
}

// celsius is written by a marshaler passed with WithJSONOptions.
type celsius float64

func json2Marshalers() json.Options {
	return json.WithMarshalers(json.MarshalFunc(func(c celsius) ([]byte, error) {
		return json.Marshal(strconv.FormatFloat(float64(c), 'g', -1, 64) + "°C")
	}))
}
