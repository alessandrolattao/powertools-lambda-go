package datadog

import (
	"context"
	"io"
	"os"
	"slices"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/internal/output"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/metrics/internal/disabled"
)

// DefaultNamespace is the namespace of metrics whose names Datadog receives
// as they are, without a prefix.
const DefaultNamespace = "default"

// Where the Datadog Lambda Extension lives and listens.
const (
	extensionPath   = "/opt/extensions/datadog-agent"
	extensionStatsd = "127.0.0.1:8125"
)

// coldStartMetric names the metric LogMetrics records on a cold start.
const coldStartMetric = "ColdStart"

// Provider sends metrics to Datadog: to the Datadog Lambda Extension when the
// function has it, or as JSON lines on standard output for the Datadog
// Forwarder. It is a metrics.Provider; New wraps it in a metrics.Metrics.
type Provider struct {
	namespace   string
	flushToLog  bool
	defaultTags []metrics.Dimension
	out         io.Writer
	now         func() time.Time
	disabled    bool
	// statsd is the extension's DogStatsD address, empty when the function
	// has no extension.
	statsd string
}

type config struct {
	provider     Provider
	functionName string
}

// Option configures New and NewProvider.
type Option func(*config)

// WithNamespace prefixes every metric name with namespace and a dot. It
// defaults to POWERTOOLS_METRICS_NAMESPACE, then to DefaultNamespace, which
// adds no prefix.
func WithNamespace(namespace string) Option {
	return func(c *config) { c.provider.namespace = namespace }
}

// WithFlushToLog writes the metrics to standard output, for the Datadog
// Forwarder, even when the function has the Datadog Lambda Extension. It
// defaults to DD_FLUSH_TO_LOG.
func WithFlushToLog(enabled bool) Option {
	return func(c *config) { c.provider.flushToLog = enabled }
}

// WithDefaultTag adds the tag key:value to every metric, under the tags of
// the metric itself, which win when they have the same key. Call it again
// for more tags.
func WithDefaultTag(key, value string) Option {
	return func(c *config) {
		c.provider.defaultTags = mergeTags(c.provider.defaultTags, []metrics.Dimension{{Name: key, Value: value}})
	}
}

// WithFunctionName sets the function_name tag of the cold start metric. It
// defaults to POWERTOOLS_METRICS_FUNCTION_NAME, then to the function's own
// name. NewProvider ignores it: the Metrics New returns records cold starts.
func WithFunctionName(name string) Option {
	return func(c *config) { c.functionName = name }
}

// WithWriter sends the JSON lines to w instead of standard output. Each line
// reaches w in a single Write call, one at a time.
func WithWriter(w io.Writer) Option {
	return func(c *config) { c.provider.out = w }
}

// WithClock sets the clock that timestamps metrics, time.Now by default.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.provider.now = now }
}

func newConfig(opts []Option) *config {
	c := &config{provider: Provider{
		namespace:  env.Get(env.MetricsNamespace),
		flushToLog: env.Bool(env.DatadogFlushToLog, false),
		now:        time.Now,
	}}
	for _, opt := range opts {
		opt(c)
	}
	p := &c.provider
	if p.namespace == "" {
		p.namespace = DefaultNamespace
	}
	p.out = output.For(p.out)
	p.disabled = disabled.Is()
	if _, err := os.Stat(extensionPath); err == nil {
		p.statsd = extensionStatsd
	}
	return c
}

// NewProvider returns the Datadog provider configured by opts, for
// metrics.New(metrics.WithProvider(...)).
func NewProvider(opts ...Option) *Provider {
	c := newConfig(opts)
	return &c.provider
}

// New returns a Metrics that sends its metrics to Datadog. Decorate the
// handler with its LogMetrics method and add metrics with AddMetric, as with
// CloudWatch EMF.
func New(opts ...Option) *metrics.Metrics {
	c := newConfig(opts)
	mopts := []metrics.Option{metrics.WithProvider(&c.provider)}
	if c.functionName != "" {
		mopts = append(mopts, metrics.WithFunctionName(c.functionName))
	}
	return metrics.New(mopts...)
}

// NewSet returns an empty metric set whose metrics carry the provider's
// default tags, then defaults.
func (p *Provider) NewSet(ctx context.Context, defaults []metrics.Dimension) metrics.Set {
	s := &set{p: p, defaults: mergeTags(p.defaultTags, defaults)}
	validateTags(ctx, s.defaults)
	return s
}

// AddColdStartMetric adds the ColdStart metric, of value 1, to set, tagged
// with function_name.
func (p *Provider) AddColdStartMetric(ctx context.Context, set metrics.Set, functionName string) error {
	return set.AddMetric(ctx, metrics.Metric{
		Name:       coldStartMetric,
		Unit:       metrics.NoUnit,
		Value:      1,
		Resolution: metrics.Standard,
		Tags:       []metrics.Dimension{{Name: "function_name", Value: functionName}},
	})
}

// AddMetric adds a data point to the metric set ctx carries, like
// metrics.AddMetric without a unit, which Datadog does not have. Tag it with
// WithTag, timestamp it with WithTimestamp.
func AddMetric(ctx context.Context, name string, value float64, opts ...metrics.MetricOption) error {
	return metrics.AddMetric(ctx, name, metrics.NoUnit, value, opts...)
}

// WithTag tags the data point with key:value. Call it again for more tags;
// a key given twice keeps its first place and its last value.
func WithTag(key, value string) metrics.MetricOption {
	return func(m *metrics.Metric) {
		m.Tags = mergeTags(m.Tags, []metrics.Dimension{{Name: key, Value: value}})
	}
}

// WithTimestamp sets the time of the data point, now by default. Datadog
// keeps it to the second.
func WithTimestamp(t time.Time) metrics.MetricOption {
	return func(m *metrics.Metric) { m.Timestamp = t }
}

// mergeTags returns base with add applied in order: a key already there
// keeps its place and takes the new value, a new key goes last, as Python's
// dictionary merge does.
func mergeTags(base, add []metrics.Dimension) []metrics.Dimension {
	out := slices.Clone(base)
	for _, t := range add {
		if i := slices.IndexFunc(out, func(o metrics.Dimension) bool { return o.Name == t.Name }); i >= 0 {
			out[i].Value = t.Value
			continue
		}
		out = append(out, t)
	}
	return out
}
