package metrics_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/metrics/internal/warn"
)

// documents parses the EMF documents written to out, one per line.
func documents(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for line := range strings.Lines(out.String()) {
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &doc), line)
		docs = append(docs, doc)
	}
	return docs
}

// directive returns the CloudWatchMetrics directive of doc.
func directive(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	aws, ok := doc["_aws"].(map[string]any)
	require.True(t, ok)
	directives, ok := aws["CloudWatchMetrics"].([]any)
	require.True(t, ok)
	require.Len(t, directives, 1)
	d, ok := directives[0].(map[string]any)
	require.True(t, ok)
	return d
}

func handler(fn func(ctx context.Context) error) func(context.Context, string) (string, error) {
	return func(ctx context.Context, event string) (string, error) {
		return event, fn(ctx)
	}
}

func TestAddMetricValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		metric string
		unit   metrics.Unit
		value  float64
		opts   []metrics.MetricOption
		want   error
	}{
		{name: "empty name", metric: "", unit: metrics.Count, value: 1, want: metrics.ErrMetricName},
		{name: "blank name", metric: "   ", unit: metrics.Count, value: 1, want: metrics.ErrMetricName},
		{name: "long name", metric: strings.Repeat("x", 256), unit: metrics.Count, value: 1, want: metrics.ErrMetricName},
		{name: "longest name", metric: strings.Repeat("é", 255), unit: metrics.Count, value: 1},
		{name: "NaN", metric: "m", unit: metrics.Count, value: math.NaN(), want: metrics.ErrMetricValue},
		{name: "infinity", metric: "m", unit: metrics.Count, value: math.Inf(-1), want: metrics.ErrMetricValue},
		{name: "unknown unit", metric: "m", unit: "incorrect_unit", value: 1, want: metrics.ErrMetricUnit},
		{name: "unit in lower case", metric: "m", unit: "count", value: 1, want: metrics.ErrMetricUnit},
		{name: "unit by member name", metric: "m", unit: "CountPerSecond", value: 1},
		{
			name: "resolution 10", metric: "m", unit: metrics.Count, value: 1,
			opts: []metrics.MetricOption{metrics.WithResolution(10)}, want: metrics.ErrMetricResolution,
		},
		{
			name: "resolution 0", metric: "m", unit: metrics.Count, value: 1,
			opts: []metrics.MetricOption{metrics.WithResolution(0)}, want: metrics.ErrMetricResolution,
		},
		{
			name: "high resolution", metric: "m", unit: metrics.Count, value: 1,
			opts: []metrics.MetricOption{metrics.WithResolution(metrics.High)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newGolden(metrics.WithNamespace("ns"))
			ctx := m.NewContext(t.Context())
			err := metrics.AddMetric(ctx, tt.metric, tt.unit, tt.value, tt.opts...)
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestParseUnit(t *testing.T) {
	t.Parallel()
	all := []metrics.Unit{
		metrics.Seconds, metrics.Microseconds, metrics.Milliseconds, metrics.Bytes, metrics.Kilobytes,
		metrics.Megabytes, metrics.Gigabytes, metrics.Terabytes, metrics.Bits, metrics.Kilobits, metrics.Megabits,
		metrics.Gigabits, metrics.Terabits, metrics.Percent, metrics.Count, metrics.BytesPerSecond,
		metrics.KilobytesPerSecond, metrics.MegabytesPerSecond, metrics.GigabytesPerSecond,
		metrics.TerabytesPerSecond, metrics.BitsPerSecond, metrics.KilobitsPerSecond, metrics.MegabitsPerSecond,
		metrics.GigabitsPerSecond, metrics.TerabitsPerSecond, metrics.CountPerSecond, metrics.NoUnit,
	}
	for _, u := range all {
		got, err := metrics.ParseUnit(string(u))
		require.NoError(t, err)
		assert.Equal(t, u, got)
	}
	for name, want := range map[string]metrics.Unit{
		"BytesPerSecond": metrics.BytesPerSecond,
		"NoUnit":         metrics.NoUnit,
		"CountPerSecond": metrics.CountPerSecond,
	} {
		got, err := metrics.ParseUnit(name)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := metrics.ParseUnit("Furlongs")
	require.ErrorIs(t, err, metrics.ErrMetricUnit)
	assert.Contains(t, err.Error(), `"Furlongs", expected either option: Seconds, Microseconds`)
}

func TestDimensionLimits(t *testing.T) {
	t.Parallel()
	addDims := func(t *testing.T, ctx context.Context, n int) {
		t.Helper()
		for i := range n {
			require.NoError(t, metrics.AddDimension(ctx, "dim"+string(rune('a'+i)), "v"))
		}
	}
	t.Run("29 dimensions", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		ctx := m.NewContext(t.Context())
		addDims(t, ctx, metrics.MaxDimensions)
		require.ErrorIs(t, metrics.AddDimension(ctx, "one_more", "v"), metrics.ErrTooManyDimensions)
		// Changing a dimension at the limit is no new dimension.
		require.NoError(t, metrics.AddDimension(ctx, "dima", "w"))
		require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
		require.NoError(t, metrics.Flush(ctx))
		dims, ok := directive(t, documents(t, out)[0])["Dimensions"].([]any)
		require.True(t, ok)
		assert.Len(t, dims[0], metrics.MaxDimensions)
	})
	t.Run("the service dimension counts", func(t *testing.T) {
		t.Parallel()
		m, _ := newGolden(metrics.WithNamespace("ns"), metrics.WithService("svc"))
		ctx := m.NewContext(t.Context())
		addDims(t, ctx, metrics.MaxDimensions)
		require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
		require.ErrorIs(t, metrics.Flush(ctx), metrics.ErrTooManyDimensions)
	})
	t.Run("dimension sets count all keys", func(t *testing.T) {
		t.Parallel()
		m, _ := newGolden(metrics.WithNamespace("ns"))
		ctx := m.NewContext(t.Context())
		addDims(t, ctx, metrics.MaxDimensions)
		err := metrics.AddDimensions(ctx, dim("extra1", "v"), dim("extra2", "v"))
		require.ErrorIs(t, err, metrics.ErrTooManyDimensions)
		require.ErrorIs(t, err, metrics.ErrSchemaValidation)
	})
	t.Run("too many default dimensions", func(t *testing.T) {
		t.Parallel()
		defaults := make([]metrics.Dimension, metrics.MaxDimensions+1)
		for i := range defaults {
			defaults[i] = dim("d"+string(rune('a'+i)), "v")
		}
		m, _ := newGolden(metrics.WithNamespace("ns"), metrics.WithDefaultDimensions(defaults...))
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			return metrics.AddMetric(ctx, "m", metrics.Count, 1)
		}))
		_, err := h(t.Context(), "")
		require.ErrorIs(t, err, metrics.ErrTooManyDimensions)
	})
}

func TestFlushErrors(t *testing.T) {
	t.Parallel()
	t.Run("no namespace", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace(""))
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			return metrics.AddMetric(ctx, "m", metrics.Count, 1)
		}))
		_, err := h(t.Context(), "")
		require.ErrorIs(t, err, metrics.ErrNoNamespace)
		assert.Empty(t, out.String())
	})
	t.Run("raise on empty metrics", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"), metrics.WithService("svc"))
		h := m.LogMetrics(handler(func(context.Context) error { return nil }), metrics.WithRaiseOnEmptyMetrics(true))
		_, err := h(t.Context(), "")
		require.ErrorIs(t, err, metrics.ErrNoMetrics)
		assert.Empty(t, out.String())
	})
	t.Run("the handler's error wins", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(context.Context) error { return boom }), metrics.WithRaiseOnEmptyMetrics(true))
		_, err := h(t.Context(), "")
		require.ErrorIs(t, err, boom)
		require.NotErrorIs(t, err, metrics.ErrNoMetrics)
		assert.Empty(t, out.String())
	})
	t.Run("flush without a set", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, metrics.Flush(t.Context()), metrics.ErrNoMetricSet)
	})
	t.Run("metadata that is not JSON", func(t *testing.T) {
		t.Parallel()
		m, _ := newGolden(metrics.WithNamespace("ns"))
		ctx := m.NewContext(t.Context())
		require.ErrorIs(t, metrics.AddMetadata(ctx, "ch", make(chan int)), metrics.ErrMetadata)
		require.ErrorIs(t, metrics.AddMetadata(ctx, "nan", math.NaN()), metrics.ErrMetadata)
	})
	t.Run("the writer fails", func(t *testing.T) {
		t.Parallel()
		m := metrics.New(metrics.WithNamespace("ns"), metrics.WithWriter(failingWriter{}))
		ctx := m.NewContext(t.Context())
		require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
		require.ErrorIs(t, metrics.Flush(ctx), errWrite)
	})
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// TestFlushOnFailure checks that metrics are flushed when the handler fails
// or panics, and that the panic goes on.
func TestFlushOnFailure(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		boom := errors.New("bubble up")
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
			return boom
		}))
		_, err := h(t.Context(), "")
		require.ErrorIs(t, err, boom)
		require.Len(t, documents(t, out), 1)
	})
	t.Run("panic", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
			panic("handler panic")
		}))
		require.PanicsWithValue(t, "handler panic", func() { _, _ = h(t.Context(), "") }) //nolint:errcheck // it panics
		require.Len(t, documents(t, out), 1)
	})
}

// TestInvocationsAreIsolated checks what Python leaks from one invocation to
// the next: dimensions and metadata of an invocation without metrics, and
// the timestamp.
func TestInvocationsAreIsolated(t *testing.T) {
	t.Parallel()
	m, out := newGolden(metrics.WithNamespace("ns"))
	first := m.LogMetrics(handler(func(ctx context.Context) error {
		require.NoError(t, metrics.AddDimension(ctx, "leaked", "x"))
		require.NoError(t, metrics.AddMetadata(ctx, "meta", "x"))
		return metrics.SetTimestamp(ctx, frozen.Add(-time.Hour))
	}))
	second := m.LogMetrics(handler(func(ctx context.Context) error {
		return metrics.AddMetric(ctx, "m", metrics.Count, 1)
	}))
	_, err := first(t.Context(), "")
	require.NoError(t, err)
	_, err = second(t.Context(), "")
	require.NoError(t, err)
	docs := documents(t, out)
	require.Len(t, docs, 1)
	assert.NotContains(t, docs[0], "leaked")
	assert.NotContains(t, docs[0], "meta")
	aws, ok := docs[0]["_aws"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, float64(frozen.UnixMilli()), aws["Timestamp"], 0.5)
}

func TestClearMetrics(t *testing.T) {
	t.Parallel()
	m, out := newGolden(metrics.WithNamespace("ns"), metrics.WithDefaultDimensions(dim("env", "prod")))
	ctx := m.NewContext(t.Context())
	require.NoError(t, metrics.AddMetric(ctx, "dropped", metrics.Count, 1))
	require.NoError(t, metrics.AddDimension(ctx, "dropped_dim", "x"))
	metrics.ClearMetrics(ctx)
	require.NoError(t, metrics.AddMetric(ctx, "kept", metrics.Count, 1))
	require.NoError(t, metrics.Flush(ctx))
	docs := documents(t, out)
	require.Len(t, docs, 1)
	assert.Equal(t, "prod", docs[0]["env"])
	assert.NotContains(t, docs[0], "dropped")
	assert.NotContains(t, docs[0], "dropped_dim")
	assert.Contains(t, docs[0], "kept")
}

func TestDefaultDimensions(t *testing.T) {
	t.Parallel()
	m, out := newGolden(metrics.WithNamespace("ns"),
		metrics.WithDefaultDimensions(dim("env", "test"), dim("team", "a")))
	ctx := m.NewContext(t.Context(), metrics.WithDefaultDimensions(dim("env", "prod"), dim("tier", "1")))
	require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
	require.NoError(t, metrics.Flush(ctx))

	cleared := m.NewContext(t.Context(), metrics.WithoutDefaultDimensions(), metrics.WithDefaultDimensions(dim("only", "me")))
	require.NoError(t, metrics.AddMetric(cleared, "m", metrics.Count, 1))
	require.NoError(t, metrics.Flush(cleared))

	docs := documents(t, out)
	require.Len(t, docs, 2)
	assert.Equal(t, []any{[]any{"env", "team", "tier"}}, directive(t, docs[0])["Dimensions"])
	assert.Equal(t, "prod", docs[0]["env"])
	assert.Equal(t, []any{[]any{"only"}}, directive(t, docs[1])["Dimensions"])
}

func TestNestedSets(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	outer := metrics.New(metrics.WithNamespace("outer"), metrics.WithWriter(&out), metrics.WithClock(frozenClock))
	inner := metrics.New(metrics.WithNamespace("inner"), metrics.WithWriter(&out), metrics.WithClock(frozenClock))
	h := outer.LogMetrics(inner.LogMetrics(handler(func(ctx context.Context) error {
		// The package function writes to the innermost set, a method to
		// its own Metrics' set.
		require.NoError(t, metrics.AddMetric(ctx, "to_inner", metrics.Count, 1))
		return outer.AddMetric(ctx, "to_outer", metrics.Count, 1)
	})))
	_, err := h(t.Context(), "")
	require.NoError(t, err)
	docs := documents(t, &out)
	require.Len(t, docs, 2)
	assert.Equal(t, "inner", directive(t, docs[0])["Namespace"])
	assert.Contains(t, docs[0], "to_inner")
	assert.Equal(t, "outer", directive(t, docs[1])["Namespace"])
	assert.Contains(t, docs[1], "to_outer")
}

func TestSingleMetric(t *testing.T) {
	t.Parallel()
	t.Run("written once", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		single := m.SingleMetric("m", metrics.Count, 1)
		require.NoError(t, single.Flush())
		require.ErrorIs(t, single.Flush(), metrics.ErrNoMetrics)
		assert.Len(t, documents(t, out), 1)
	})
	t.Run("the first error stops the chain", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		err := m.SingleMetric("", metrics.Count, 1).AddDimension("a", "b").SetTimestamp(frozen).Flush()
		require.ErrorIs(t, err, metrics.ErrMetricName)
		err = m.SingleMetric("m", metrics.Count, 1).AddMetadata("ch", make(chan int)).AddDimension("a", "b").Flush()
		require.ErrorIs(t, err, metrics.ErrMetadata)
		assert.Empty(t, out.String())
	})
	t.Run("another provider", func(t *testing.T) {
		t.Parallel()
		m := metrics.New(metrics.WithProvider(nopProvider{}))
		require.ErrorIs(t, m.SingleMetric("m", metrics.Count, 1).Flush(), errors.ErrUnsupported)
	})
}

// nopProvider is a provider whose sets keep nothing.
type nopProvider struct{}

func (nopProvider) NewSet(context.Context, []metrics.Dimension) metrics.Set { return &countingSet{} }

func (nopProvider) AddColdStartMetric(ctx context.Context, set metrics.Set, _ string) error {
	return set.AddMetric(ctx, metrics.Metric{Name: "ColdStart"})
}

// countingSet counts the calls a provider's set receives.
type countingSet struct {
	metrics []metrics.Metric
	flushes int
}

func (s *countingSet) AddMetric(_ context.Context, m metrics.Metric) error {
	s.metrics = append(s.metrics, m)
	return nil
}

func (*countingSet) AddDimension(context.Context, string, string) error { return nil }

func (*countingSet) AddDimensions(context.Context, []metrics.Dimension) error { return nil }

func (*countingSet) AddMetadata(context.Context, string, any) error { return nil }

func (*countingSet) SetTimestamp(context.Context, time.Time) error { return nil }

func (s *countingSet) Flush(context.Context, bool) error {
	s.flushes++
	s.metrics = nil
	return nil
}

func (s *countingSet) Clear() { s.metrics = nil }

func (s *countingSet) Len() int { return len(s.metrics) }

// recordingProvider hands out countingSets and keeps them.
type recordingProvider struct {
	sets []*countingSet
}

func (p *recordingProvider) NewSet(context.Context, []metrics.Dimension) metrics.Set {
	s := &countingSet{}
	p.sets = append(p.sets, s)
	return s
}

func (*recordingProvider) AddColdStartMetric(ctx context.Context, set metrics.Set, name string) error {
	return set.AddMetric(ctx, metrics.Metric{Name: "ColdStart", Tags: []metrics.Dimension{dim("function_name", name)}})
}

func TestCustomProvider(t *testing.T) {
	t.Parallel()
	p := &recordingProvider{}
	m := metrics.New(metrics.WithProvider(p))
	h := m.LogMetrics(handler(func(ctx context.Context) error {
		return metrics.AddMetric(ctx, "m", metrics.Count, 2, metrics.WithResolution(metrics.High))
	}))
	_, err := h(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, p.sets, 1)
	assert.Equal(t, 1, p.sets[0].flushes)
}

// TestPendingSet covers the methods of a Metrics called with a context that
// carries none of its sets.
func TestPendingSet(t *testing.T) {
	t.Parallel()
	t.Run("flushed with the next invocation", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		require.NoError(t, m.AddDimension(context.Background(), "scope", "init"))
		require.NoError(t, m.AddMetric(context.Background(), "ConfigLoaded", metrics.Count, 1))
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			return m.AddMetric(ctx, "Invoked", metrics.Count, 1)
		}))
		_, err := h(t.Context(), "")
		require.NoError(t, err)
		docs := documents(t, out)
		require.Len(t, docs, 2)
		assert.Equal(t, "init", docs[0]["scope"])
		assert.Contains(t, docs[0], "ConfigLoaded")
		assert.NotContains(t, docs[1], "scope")
		assert.Contains(t, docs[1], "Invoked")
	})
	t.Run("flushed by Flush", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		require.NoError(t, m.AddMetric(context.Background(), "m", metrics.Count, 1))
		require.NoError(t, m.Flush(context.Background()))
		assert.Len(t, documents(t, out), 1)
		m.ClearMetrics(context.Background())
	})
	t.Run("cleared", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		require.NoError(t, m.AddMetric(context.Background(), "m", metrics.Count, 1))
		m.ClearMetrics(context.Background())
		require.NoError(t, m.Flush(context.Background()))
		assert.Empty(t, out.String())
	})
	t.Run("flushed when full", func(t *testing.T) {
		t.Parallel()
		m, out := newGolden(metrics.WithNamespace("ns"))
		for i := range metrics.MaxMetrics {
			require.NoError(t, m.AddMetric(context.Background(), "m", metrics.Count, float64(i)))
		}
		docs := documents(t, out)
		require.Len(t, docs, 1)
		assert.Len(t, docs[0]["m"], metrics.MaxMetrics)
	})
	t.Run("flushed when full, another provider", func(t *testing.T) {
		t.Parallel()
		p := &recordingProvider{}
		m := metrics.New(metrics.WithProvider(p))
		for range metrics.MaxMetrics {
			require.NoError(t, m.AddMetric(context.Background(), "m", metrics.Count, 1))
		}
		require.Len(t, p.sets, 1)
		assert.Equal(t, 1, p.sets[0].flushes)
	})
}

// TestOrphanMetrics covers the package functions called with a context that
// carries no metric set. The orphans are process-wide, so it runs alone.
func TestOrphanMetrics(t *testing.T) { //nolint:paralleltest // the orphan set is process-wide
	metrics.ClearMetrics(context.Background())
	defer metrics.ClearMetrics(context.Background())

	require.NoError(t, metrics.AddDimension(context.Background(), "scope", "init"))
	m, out := newGolden(metrics.WithNamespace("ns"))
	ctx := m.NewContext(t.Context())
	require.NoError(t, metrics.AddMetric(ctx, "in_set", metrics.Count, 1))
	require.NoError(t, metrics.Flush(ctx))
	docs := documents(t, out)
	require.Len(t, docs, 1, "dimensions alone wait for a metric")

	require.NoError(t, metrics.AddMetric(context.Background(), "orphan", metrics.Count, 1))
	require.ErrorIs(t, metrics.AddMetric(context.Background(), "nan", metrics.Count, math.NaN()), metrics.ErrMetricValue)
	out.Reset()
	ctx = m.NewContext(t.Context())
	require.NoError(t, metrics.AddMetric(ctx, "in_set", metrics.Count, 1))
	require.NoError(t, metrics.Flush(ctx))
	docs = documents(t, out)
	require.Len(t, docs, 2)
	assert.Equal(t, "init", docs[0]["scope"])
	assert.Contains(t, docs[0], "orphan")
	assert.NotContains(t, docs[1], "scope")

	for range metrics.MaxMetrics {
		require.NoError(t, metrics.AddMetric(context.Background(), "orphan", metrics.Count, 1))
	}
	require.ErrorIs(t, metrics.AddMetric(context.Background(), "orphan", metrics.Count, 1), metrics.ErrNoMetricSet)
}

// TestDisabled checks the switches of Powertools Python, and that nothing at
// all is written when metrics are off: not the cold start metric, not a
// full document, not a single metric.
func TestDisabled(t *testing.T) {
	tests := []struct {
		name     string
		disabled string
		dev      string
		want     bool
	}{
		{name: "disabled", disabled: "true", want: false},
		{name: "enabled explicitly", disabled: "false", want: true},
		{name: "dev mode", dev: "true", want: false},
		{name: "dev mode, enabled explicitly", dev: "true", disabled: "false", want: true},
		{name: "dev mode off", dev: "false", want: true},
		{name: "disabled, dev mode off", dev: "false", disabled: "1", want: false},
		{name: "nothing set", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POWERTOOLS_METRICS_DISABLED", tt.disabled)
			t.Setenv("POWERTOOLS_DEV", tt.dev)
			coldstart.Reset()
			m, out := newGolden(metrics.WithNamespace("ns"), metrics.WithService("svc"))
			h := m.LogMetrics(handler(func(ctx context.Context) error {
				for i := range metrics.MaxMetrics + 1 {
					if err := metrics.AddMetric(ctx, "m", metrics.Count, float64(i)); err != nil {
						return err
					}
				}
				return nil
			}), metrics.WithColdStartMetric(true))
			_, err := h(t.Context(), "")
			require.NoError(t, err)
			require.NoError(t, m.SingleMetric("single", metrics.Count, 1).Flush())
			if tt.want {
				assert.Len(t, documents(t, out), 4)
			} else {
				assert.Empty(t, out.String())
			}
		})
	}
}

// TestColdStart checks the cold start metric: once per process, also when
// the cold invocation fails, never with provisioned concurrency.
func TestColdStart(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "fn")
	t.Setenv("POWERTOOLS_METRICS_FUNCTION_NAME", "")
	t.Run("failed invocation", func(t *testing.T) { //nolint:paralleltest // the cold start claim is process-wide
		coldstart.Reset()
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(context.Context) error { return errors.New("cold failure") }),
			metrics.WithColdStartMetric(true))
		_, err := h(t.Context(), "")
		require.Error(t, err)
		docs := documents(t, out)
		require.Len(t, docs, 1)
		assert.Equal(t, "fn", docs[0]["function_name"])
		assert.Equal(t, []any{1.0}, docs[0]["ColdStart"])
	})
	t.Run("panicking invocation", func(t *testing.T) { //nolint:paralleltest // the cold start claim is process-wide
		coldstart.Reset()
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(context.Context) error { panic("cold panic") }), metrics.WithColdStartMetric(true))
		require.Panics(t, func() { _, _ = h(t.Context(), "") }) //nolint:errcheck // it panics
		assert.Len(t, documents(t, out), 1)
	})
	t.Run("environment variable", func(t *testing.T) {
		coldstart.Reset()
		t.Setenv("POWERTOOLS_METRICS_FUNCTION_NAME", "from_env")
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(context.Context) error { return nil }), metrics.WithColdStartMetric(true))
		_, err := h(t.Context(), "")
		require.NoError(t, err)
		assert.Equal(t, "from_env", documents(t, out)[0]["function_name"])
	})
	t.Run("provisioned concurrency", func(t *testing.T) {
		coldstart.Reset()
		t.Setenv("AWS_LAMBDA_INITIALIZATION_TYPE", "provisioned-concurrency")
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			return metrics.AddMetric(ctx, "m", metrics.Count, 1)
		}), metrics.WithColdStartMetric(true))
		_, err := h(t.Context(), "")
		require.NoError(t, err)
		docs := documents(t, out)
		require.Len(t, docs, 1)
		assert.NotContains(t, docs[0], "ColdStart")
	})
	t.Run("not captured", func(t *testing.T) { //nolint:paralleltest // the cold start claim is process-wide
		coldstart.Reset()
		m, out := newGolden(metrics.WithNamespace("ns"))
		h := m.LogMetrics(handler(func(ctx context.Context) error {
			return metrics.AddMetric(ctx, "m", metrics.Count, 1)
		}))
		_, err := h(t.Context(), "")
		require.NoError(t, err)
		assert.Len(t, documents(t, out), 1)
	})
}

func TestEnvironment(t *testing.T) {
	t.Setenv("POWERTOOLS_METRICS_NAMESPACE", "from_env")
	t.Setenv("POWERTOOLS_SERVICE_NAME", "svc_env")
	m, out := newGolden()
	ctx := m.NewContext(t.Context())
	require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
	require.NoError(t, metrics.Flush(ctx))

	explicit, explicitOut := newGolden(metrics.WithNamespace("explicit"), metrics.WithService("svc"))
	ctx = explicit.NewContext(t.Context())
	require.NoError(t, metrics.AddMetric(ctx, "m", metrics.Count, 1))
	require.NoError(t, metrics.Flush(ctx))

	doc := documents(t, out)[0]
	assert.Equal(t, "from_env", directive(t, doc)["Namespace"])
	assert.Equal(t, "svc_env", doc["service"])
	doc = documents(t, explicitOut)[0]
	assert.Equal(t, "explicit", directive(t, doc)["Namespace"])
	assert.Equal(t, "svc", doc["service"])
}

// TestWarnings checks the warnings Powertools Python gives, once each.
func TestWarnings(t *testing.T) { //nolint:paralleltest // warnings are captured process-wide
	var logs bytes.Buffer
	restore := warn.Capture(&logs)
	defer restore()

	m, _ := newGolden(metrics.WithNamespace("ns"))
	h := m.LogMetrics(handler(func(context.Context) error { return nil }))
	for range 2 {
		_, err := h(t.Context(), "")
		require.NoError(t, err)
	}
	ctx := m.NewContext(t.Context())
	require.NoError(t, metrics.AddDimension(ctx, "env", "a"))
	require.NoError(t, metrics.AddDimension(ctx, "env", "a"))
	require.NoError(t, metrics.AddDimension(ctx, "env", "b"))
	require.NoError(t, metrics.AddDimension(ctx, "", "x"))
	require.NoError(t, metrics.AddDimensions(ctx))
	require.NoError(t, metrics.AddDimensions(ctx, dim("k", "")))
	require.NoError(t, metrics.SetTimestamp(ctx, frozen.Add(-20*24*time.Hour)))
	require.NoError(t, metrics.SetTimestamp(ctx, frozen.Add(3*time.Hour)))
	require.NoError(t, metrics.SetTimestamp(ctx, frozen.Add(-24*time.Hour)))

	var records []map[string]any
	for line := range strings.Lines(logs.String()) {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		assert.Equal(t, "WARN", record["level"])
		records = append(records, record)
	}
	messages := make([]any, len(records))
	for i, r := range records {
		messages[i] = r["msg"]
	}
	assert.Equal(t, []any{
		warn.NoMetrics,
		"Dimension 'env' has already been added. The previous value will be overwritten.",
		"The dimension  doesn't meet the requirements and won't be added. " +
			"Ensure the dimension name and value are non-empty strings",
		"Empty dimensions dictionary provided",
		"Dimension k has empty name or value",
		"This metric doesn't meet the requirements and will be skipped by Amazon CloudWatch. " +
			"Ensure the timestamp is within 14 days past or 2 hours future.",
	}, messages)
	assert.Equal(t, "UserWarning", records[0]["category"])
	assert.Equal(t, "PowertoolsUserWarning", records[1]["category"])
}
