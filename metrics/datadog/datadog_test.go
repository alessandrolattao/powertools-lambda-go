package datadog_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/metrics/datadog"
)

// frozen is the time ../testdata/golden/generate.py freezes Powertools
// Python's clock at.
var frozen = time.Unix(1700000000, 500_000_000)

func frozenClock() time.Time { return frozen }

func golden(t *testing.T, scenario string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", scenario+".jsonl"))
	require.NoError(t, err)
	return string(data)
}

// newForwarder returns a Metrics writing Datadog Forwarder lines to a
// buffer, on the frozen clock.
func newForwarder(opts ...datadog.Option) (*metrics.Metrics, *bytes.Buffer) {
	var out bytes.Buffer
	opts = append([]datadog.Option{
		datadog.WithFlushToLog(true),
		datadog.WithWriter(&out),
		datadog.WithClock(frozenClock),
	}, opts...)
	return datadog.New(opts...), &out
}

func invoke(t *testing.T, m *metrics.Metrics, fn func(ctx context.Context) error, opts ...metrics.LogOption) error {
	t.Helper()
	h := m.LogMetrics(func(ctx context.Context, _ struct{}) (struct{}, error) {
		return struct{}{}, fn(ctx)
	}, opts...)
	_, err := h(t.Context(), struct{}{})
	return err
}

func TestGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		scenario string
		opts     []datadog.Option
		logOpts  []metrics.LogOption
		run      func(ctx context.Context) error
	}{
		{
			scenario: "tags",
			run: func(ctx context.Context) error {
				return datadog.AddMetric(ctx, "SuccessfulBooking", 1,
					datadog.WithTag("tag1", "powertools"), datadog.WithTag("tag2", "python"))
			},
		},
		{
			scenario: "namespace",
			opts:     []datadog.Option{datadog.WithNamespace("ServerlessAirline")},
			run: func(ctx context.Context) error {
				err := datadog.AddMetric(ctx, "item_sold", 12.45,
					datadog.WithTag("product", "latte"), datadog.WithTag("order", "online"))
				return errors.Join(err, datadog.AddMetric(ctx, "item_sold", 3))
			},
		},
		{
			scenario: "default_tags",
			logOpts: []metrics.LogOption{metrics.WithDefaultDimensions(
				metrics.Dimension{Name: "environment", Value: "test"},
				metrics.Dimension{Name: "log_group", Value: "/lambda/test"},
			)},
			run: func(ctx context.Context) error {
				return datadog.AddMetric(ctx, "item_sold", 1,
					datadog.WithTag("environment", "metric_precedence"), datadog.WithTag("product", "powertools"))
			},
		},
		{
			scenario: "timestamp",
			run: func(ctx context.Context) error {
				return datadog.AddMetric(ctx, "SuccessfulBooking", 1, datadog.WithTimestamp(time.Unix(1691768022, 0)))
			},
		},
		{
			scenario: "non_ascii",
			run: func(ctx context.Context) error {
				return datadog.AddMetric(ctx, "orders", 2, datadog.WithTag("city", "Zürich"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			t.Parallel()
			m, out := newForwarder(tt.opts...)
			require.NoError(t, invoke(t, m, tt.run, tt.logOpts...))
			assert.Equal(t, golden(t, tt.scenario), out.String())
		})
	}
}

// TestColdStart compares the cold start metric, added to the invocation's
// metrics as Python adds it. The cold start is claimed once per process.
func TestColdStart(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "HelloWorldFunction")
	t.Setenv("POWERTOOLS_METRICS_FUNCTION_NAME", "")
	coldstart.Reset()
	m, out := newForwarder()
	run := func(ctx context.Context) error {
		return datadog.AddMetric(ctx, "SuccessfulBooking", 1, datadog.WithTag("tag1", "powertools"))
	}
	require.NoError(t, invoke(t, m, run, metrics.WithColdStartMetric(true)))
	assert.Equal(t, golden(t, "cold_start"), out.String())

	out.Reset()
	require.NoError(t, invoke(t, m, run, metrics.WithColdStartMetric(true)))
	assert.NotContains(t, out.String(), "ColdStart")

	coldstart.Reset()
	out.Reset()
	named, namedOut := newForwarder(datadog.WithFunctionName("example_fn_constructor"))
	require.NoError(t, invoke(t, named, run, metrics.WithColdStartMetric(true)))
	assert.Contains(t, namedOut.String(), `"function_name:example_fn_constructor"`)
}

func TestValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		value float64
		want  []error
	}{
		{name: "starts with a digit", input: "1_metric", value: 1, want: []error{metrics.ErrSchemaValidation, metrics.ErrMetricName}},
		{name: "not ASCII", input: "metric_ç", value: 1, want: []error{metrics.ErrSchemaValidation, metrics.ErrMetricName}},
		{name: "too long", input: strings.Repeat("x", 201), value: 1, want: []error{metrics.ErrMetricName}},
		{name: "empty", input: "", value: 1, want: []error{metrics.ErrMetricName}},
		{name: "space", input: "item sold", value: 1, want: []error{metrics.ErrMetricName}},
		{name: "NaN", input: "item_sold", value: math.NaN(), want: []error{metrics.ErrMetricValue}},
		{name: "longest", input: strings.Repeat("x", 200), value: 1},
		{name: "dots", input: "coffee_house.order_value", value: 12.45},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newForwarder()
			err := invoke(t, m, func(ctx context.Context) error { return datadog.AddMetric(ctx, tt.input, tt.value) })
			if len(tt.want) == 0 {
				require.NoError(t, err)
				return
			}
			for _, want := range tt.want {
				require.ErrorIs(t, err, want)
			}
		})
	}
}

func TestUnsupported(t *testing.T) {
	t.Parallel()
	m, _ := newForwarder()
	ctx := m.NewContext(t.Context())
	require.ErrorIs(t, metrics.AddDimension(ctx, "a", "b"), errors.ErrUnsupported)
	require.ErrorIs(t, metrics.AddDimensions(ctx, metrics.Dimension{Name: "a", Value: "b"}), errors.ErrUnsupported)
	require.ErrorIs(t, metrics.AddMetadata(ctx, "a", "b"), errors.ErrUnsupported)
	require.ErrorIs(t, metrics.SetTimestamp(ctx, frozen), errors.ErrUnsupported)
	require.ErrorIs(t, m.SingleMetric("m", metrics.Count, 1).Flush(), errors.ErrUnsupported)
}

func TestFlush(t *testing.T) {
	t.Parallel()
	t.Run("raise on empty metrics", func(t *testing.T) {
		t.Parallel()
		m, out := newForwarder()
		err := invoke(t, m, func(context.Context) error { return nil }, metrics.WithRaiseOnEmptyMetrics(true))
		require.ErrorIs(t, err, metrics.ErrNoMetrics)
		assert.Empty(t, out.String())
	})
	t.Run("cleared after the invocation", func(t *testing.T) {
		t.Parallel()
		m, out := newForwarder()
		ctx := m.NewContext(t.Context())
		require.NoError(t, datadog.AddMetric(ctx, "first", 1))
		require.NoError(t, metrics.Flush(ctx))
		require.NoError(t, datadog.AddMetric(ctx, "second", 1))
		require.NoError(t, metrics.Flush(ctx))
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		require.Len(t, lines, 2)
		assert.Contains(t, lines[1], `"m":"second"`)
	})
	t.Run("one line per metric", func(t *testing.T) {
		t.Parallel()
		m, out := newForwarder(datadog.WithDefaultTag("env", "prod"), datadog.WithDefaultTag("team", "a"))
		require.NoError(t, invoke(t, m, func(ctx context.Context) error {
			for range 3 {
				if err := datadog.AddMetric(ctx, "m", 1, datadog.WithTag("team", "b")); err != nil {
					return err
				}
			}
			return nil
		}))
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		require.Len(t, lines, 3)
		for _, line := range lines {
			var got struct {
				M string   `json:"m"`
				T []string `json:"t"`
			}
			require.NoError(t, json.Unmarshal([]byte(line), &got))
			assert.Equal(t, "m", got.M)
			assert.Equal(t, []string{"env:prod", "team:b"}, got.T)
		}
	})
	t.Run("the writer fails", func(t *testing.T) {
		t.Parallel()
		m := datadog.New(datadog.WithFlushToLog(true), datadog.WithWriter(failingWriter{}))
		err := invoke(t, m, func(ctx context.Context) error { return datadog.AddMetric(ctx, "m", 1) })
		require.ErrorIs(t, err, errWrite)
	})
	t.Run("custom provider wiring", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		p := datadog.NewProvider(datadog.WithFlushToLog(true), datadog.WithWriter(&out), datadog.WithClock(frozenClock))
		m := metrics.New(metrics.WithProvider(p))
		require.NoError(t, invoke(t, m, func(ctx context.Context) error { return datadog.AddMetric(ctx, "m", 1) }))
		assert.JSONEq(t, `{"m":"m","v":1,"e":1700000000,"t":[]}`, out.String())
	})
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// TestEnvironment checks the variables Powertools Python reads.
func TestEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		opts []datadog.Option
		want string
	}{
		{
			name: "namespace from the environment",
			env:  map[string]string{"POWERTOOLS_METRICS_NAMESPACE": "a_namespace", "DD_FLUSH_TO_LOG": "True"},
			want: `{"m":"a_namespace.item_sold","v":1,"e":1700000000,"t":[]}` + "\n",
		},
		{
			name: "explicit namespace wins",
			env:  map[string]string{"POWERTOOLS_METRICS_NAMESPACE": "a_namespace", "DD_FLUSH_TO_LOG": "true"},
			opts: []datadog.Option{datadog.WithNamespace("explicit")},
			want: `{"m":"explicit.item_sold","v":1,"e":1700000000,"t":[]}` + "\n",
		},
		{
			name: "default namespace",
			env:  map[string]string{"DD_FLUSH_TO_LOG": "1"},
			opts: []datadog.Option{datadog.WithNamespace(datadog.DefaultNamespace)},
			want: `{"m":"item_sold","v":1,"e":1700000000,"t":[]}` + "\n",
		},
		{
			name: "disabled",
			env:  map[string]string{"POWERTOOLS_METRICS_DISABLED": "true", "DD_FLUSH_TO_LOG": "true"},
		},
		{
			name: "dev mode",
			env:  map[string]string{"POWERTOOLS_DEV": "true", "DD_FLUSH_TO_LOG": "true"},
		},
		{
			name: "dev mode, enabled explicitly",
			env:  map[string]string{"POWERTOOLS_DEV": "true", "POWERTOOLS_METRICS_DISABLED": "false", "DD_FLUSH_TO_LOG": "true"},
			want: `{"m":"item_sold","v":1,"e":1700000000,"t":[]}` + "\n",
		},
		{
			// Without the extension, the metrics go to standard output
			// whatever DD_FLUSH_TO_LOG says.
			name: "no extension",
			env:  map[string]string{"DD_FLUSH_TO_LOG": "false"},
			want: `{"m":"item_sold","v":1,"e":1700000000,"t":[]}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{"POWERTOOLS_METRICS_NAMESPACE", "POWERTOOLS_METRICS_DISABLED", "POWERTOOLS_DEV", "DD_FLUSH_TO_LOG"} {
				t.Setenv(name, tt.env[name])
			}
			var out bytes.Buffer
			opts := append([]datadog.Option{datadog.WithWriter(&out), datadog.WithClock(frozenClock)}, tt.opts...)
			m := datadog.New(opts...)
			require.NoError(t, invoke(t, m, func(ctx context.Context) error { return datadog.AddMetric(ctx, "item_sold", 1) }))
			assert.Equal(t, tt.want, out.String())
		})
	}
}
