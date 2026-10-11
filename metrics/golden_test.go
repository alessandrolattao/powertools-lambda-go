package metrics_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
)

// frozen is the time testdata/golden/generate.py freezes Powertools Python's
// clock at.
var frozen = time.Unix(1700000000, 500_000_000)

func frozenClock() time.Time { return frozen }

// golden returns the documents Powertools Python wrote for scenario.
func golden(t *testing.T, scenario string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", scenario+".jsonl"))
	require.NoError(t, err)
	return string(data)
}

// newGolden returns a Metrics writing to a buffer, on the frozen clock.
func newGolden(opts ...metrics.Option) (*metrics.Metrics, *bytes.Buffer) {
	var out bytes.Buffer
	opts = append([]metrics.Option{metrics.WithWriter(&out), metrics.WithClock(frozenClock)}, opts...)
	return metrics.New(opts...), &out
}

// invoke runs handler through m.LogMetrics with opts.
func invoke(t *testing.T, m *metrics.Metrics, handler func(ctx context.Context) error, opts ...metrics.LogOption) {
	t.Helper()
	h := m.LogMetrics(func(ctx context.Context, _ struct{}) (struct{}, error) {
		return struct{}{}, handler(ctx)
	}, opts...)
	_, err := h(t.Context(), struct{}{})
	require.NoError(t, err)
}

func dim(name, value string) metrics.Dimension {
	return metrics.Dimension{Name: name, Value: value}
}

// TestGoldenEMF replays the calls of the Python scenarios and compares the
// documents byte for byte.
func TestGoldenEMF(t *testing.T) {
	t.Parallel()
	tests := []struct {
		scenario string
		opts     []metrics.Option
		run      func(t *testing.T, m *metrics.Metrics)
	}{
		{
			scenario: "log_metrics_basic",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
				})
			},
		},
		{
			scenario: "multi_value",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "environment", "dev"))
					require.NoError(t, metrics.AddMetric(ctx, "TurbineReads", metrics.Count, 1))
					return metrics.AddMetric(ctx, "TurbineReads", metrics.Count, 8)
				})
			},
		},
		{
			scenario: "high_resolution",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1, metrics.WithResolution(metrics.High)))
					return metrics.AddMetric(ctx, "Latency", metrics.Milliseconds, 12.5)
				})
			},
		},
		{
			scenario: "two_dimensions",
			opts:     []metrics.Option{metrics.WithNamespace("test_namespace")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddMetric(ctx, "metric_one", metrics.Count, 1))
					require.NoError(t, metrics.AddMetric(ctx, "metric_two", metrics.Count, 1))
					require.NoError(t, metrics.AddDimension(ctx, "test_dimension", "test"))
					return metrics.AddDimension(ctx, "test_dimension_2", "test")
				})
			},
		},
		{
			scenario: "metadata",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1))
					require.NoError(t, metrics.AddMetadata(ctx, "booking_id", "00347014-341d-4b8e-8421-a89d3d588ab3"))
					return metrics.AddMetadata(ctx, "details", map[string]any{
						"amount": 10.5,
						"count":  3,
						"nested": map[string]any{"a": []any{1, 2.5, nil, true}, "b": 1.0},
						"text":   "café",
					})
				})
			},
		},
		{
			scenario: "dimension_sets",
			opts:     []metrics.Option{metrics.WithNamespace("TestApp")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				ctx := m.NewContext(t.Context())
				require.NoError(t, metrics.AddDimension(ctx, "service", "booking"))
				require.NoError(t, metrics.AddDimensions(ctx, dim("environment", "prod"), dim("region", "us-east-1")))
				require.NoError(t, metrics.AddDimensions(ctx, dim("environment", "prod")))
				require.NoError(t, metrics.AddDimensions(ctx, dim("region", "us-east-1")))
				require.NoError(t, metrics.AddMetric(ctx, "SuccessfulRequests", metrics.Count, 10))
				require.NoError(t, metrics.Flush(ctx))
			},
		},
		{
			scenario: "dimension_sets_with_defaults",
			opts: []metrics.Option{
				metrics.WithNamespace("TestApp"),
				metrics.WithDefaultDimensions(dim("tenant_id", "123"), dim("application", "api")),
			},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				ctx := m.NewContext(t.Context())
				require.NoError(t, metrics.AddDimensions(ctx, dim("environment", "prod")))
				require.NoError(t, metrics.AddDimensions(ctx, dim("region", "eu-west-1")))
				require.NoError(t, metrics.AddMetric(ctx, "ApiCalls", metrics.Count, 5))
				require.NoError(t, metrics.Flush(ctx))
			},
		},
		{
			scenario: "dimension_sets_last_value_wins",
			opts:     []metrics.Option{metrics.WithNamespace("TestApp")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				ctx := m.NewContext(t.Context())
				require.NoError(t, metrics.AddDimensions(ctx, dim("environment", "dev"), dim("region", "us-east-1")))
				require.NoError(t, metrics.AddDimensions(ctx, dim("environment", "staging"), dim("region", "us-west-2")))
				require.NoError(t, metrics.AddDimensions(ctx, dim("environment", "prod")))
				require.NoError(t, metrics.AddMetric(ctx, "TestMetric", metrics.Count, 1))
				require.NoError(t, metrics.Flush(ctx))
			},
		},
		{
			scenario: "default_dimensions",
			opts: []metrics.Option{
				metrics.WithNamespace("ServerlessAirline"),
				metrics.WithService("booking"),
				metrics.WithDefaultDimensions(dim("environment", "test"), dim("log_group", "/lambda/test")),
			},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				for range 2 {
					invoke(t, m, func(ctx context.Context) error {
						return metrics.AddMetric(ctx, "metric_one", metrics.Count, 1)
					})
				}
			},
		},
		{
			scenario: "single",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				require.NoError(t, m.SingleMetric("MySingleMetric", metrics.Count, 1).AddDimension("environment", "dev").Flush())
			},
		},
		{
			scenario: "single_default_dimensions",
			opts: []metrics.Option{
				metrics.WithNamespace("ServerlessAirline"),
				metrics.WithDefaultDimensions(dim("environment", "dev")),
			},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				require.NoError(t, m.SingleMetric("RecordsCount", metrics.Count, 10).
					AddDefaultDimensions().
					AddDimension("TableName", "Users").
					Flush())
			},
		},
		{
			scenario: "single_timestamp",
			opts:     []metrics.Option{metrics.WithNamespace("Powertools")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				require.NoError(t, m.SingleMetric("Orders", metrics.Count, 10).
					SetTimestamp(frozen.Add(-time.Hour)).
					AddMetadata("record_id", "6ba7b810-9dad-11d1-80b4-00c04fd430c8").
					Flush())
			},
		},
		{
			scenario: "custom_timestamp",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1))
					return metrics.SetTimestamp(ctx, frozen.Add(-48*time.Hour))
				})
			},
		},
		{
			scenario: "overflow_metrics",
			opts:     []metrics.Option{metrics.WithNamespace("test_namespace")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "test_dimension", "test"))
					for i := range 101 {
						require.NoError(t, metrics.AddMetric(ctx, "metric_"+strconv.Itoa(i), metrics.Count, 1))
					}
					return nil
				})
			},
		},
		{
			scenario: "overflow_values",
			opts:     []metrics.Option{metrics.WithNamespace("test_namespace"), metrics.WithService("test_service")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "test_dimension", "test"))
					for i := range 101 {
						require.NoError(t, metrics.AddMetric(ctx, "metric", metrics.Count, float64(i)))
					}
					return nil
				})
			},
		},
		{
			scenario: "renamed_service",
			opts:     []metrics.Option{metrics.WithNamespace("test_application"), metrics.WithService("test_service")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "service", "another_test_service"))
					return metrics.AddMetric(ctx, "single_metric", metrics.Count, 1)
				})
			},
		},
		{
			scenario: "units_by_name",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddMetric(ctx, "Throughput", "BytesPerSecond", 2048))
					require.NoError(t, metrics.AddMetric(ctx, "Ratio", "NoUnit", 0.5))
					return metrics.AddMetric(ctx, "Rate", "Count/Second", 3)
				})
			},
		},
		{
			scenario: "float_formats",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					negativeZero := 0.0
					negativeZero = -negativeZero
					for _, v := range []float64{0.1, 1e16, 1e15, 1e-05, 0.0001, 123456789.123, negativeZero, 1.5e300, 100, -2.5, 1.0 / 3} {
						require.NoError(t, metrics.AddMetric(ctx, "Values", metrics.Count, v))
					}
					return nil
				})
			},
		},
		{
			scenario: "non_ascii",
			opts:     []metrics.Option{metrics.WithNamespace("Café")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "city", "Zürich ☕ 😀"))
					require.NoError(t, metrics.AddMetric(ctx, "Ünits", metrics.Count, 1))
					return metrics.AddMetadata(ctx, "note", "tab\there \"quoted\" \\ back\x7fdel \u2028")
				})
			},
		},
		{
			scenario: "metadata_overrides_dimension",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "environment", "prod"))
					require.NoError(t, metrics.AddMetadata(ctx, "environment", "from-metadata"))
					return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
				})
			},
		},
		{
			scenario: "empty_dimension_skipped",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline")},
			run: func(t *testing.T, m *metrics.Metrics) {
				t.Helper()
				invoke(t, m, func(ctx context.Context) error {
					require.NoError(t, metrics.AddDimension(ctx, "my_empty_dimension", " "))
					require.NoError(t, metrics.AddDimension(ctx, "environment", "prod"))
					return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			t.Parallel()
			m, out := newGolden(tt.opts...)
			tt.run(t, m)
			assert.Equal(t, golden(t, tt.scenario), out.String())
		})
	}
}

// TestGoldenColdStart compares the cold start documents. The cold start is
// claimed once per process, so these scenarios run one at a time.
func TestGoldenColdStart(t *testing.T) {
	tests := []struct {
		scenario string
		opts     []metrics.Option
		run      func(ctx context.Context) error
		calls    int
	}{
		{
			scenario: "cold_start",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking")},
			run: func(ctx context.Context) error {
				if err := metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1); err != nil {
					return err
				}
				return metrics.AddDimension(ctx, "test_dimension", "test")
			},
			calls: 2,
		},
		{
			scenario: "cold_start_no_service",
			opts:     []metrics.Option{metrics.WithNamespace("ServerlessAirline")},
			run: func(ctx context.Context) error {
				return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
			},
			calls: 1,
		},
		{
			scenario: "cold_start_function_name",
			opts: []metrics.Option{
				metrics.WithNamespace("ServerlessAirline"),
				metrics.WithService("booking"),
				metrics.WithFunctionName("example_fn_constructor"),
			},
			run: func(ctx context.Context) error {
				return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
			},
			calls: 1,
		},
	}
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "test")
	t.Setenv("POWERTOOLS_METRICS_FUNCTION_NAME", "")
	for _, tt := range tests { //nolint:paralleltest // the cold start claim is process-wide
		t.Run(tt.scenario, func(t *testing.T) {
			coldstart.Reset()
			m, out := newGolden(tt.opts...)
			for range tt.calls {
				invoke(t, m, tt.run, metrics.WithColdStartMetric(true))
			}
			assert.Equal(t, golden(t, tt.scenario), out.String())
		})
	}
}
