package metrics

import (
	"context"
	"time"
)

// Dimension is a name and a value: a CloudWatch dimension, or a Datadog tag.
// Ordered lists of dimensions are slices, so that documents list them in the
// order they were given.
type Dimension struct {
	Name  string
	Value string
}

// Metric is one data point, as AddMetric hands it to the metric set.
type Metric struct {
	// Name identifies the metric.
	Name string
	// Unit is the CloudWatch unit. Datadog has no units and ignores it.
	Unit Unit
	// Value is the measurement, a finite number.
	Value float64
	// Resolution is the CloudWatch storage resolution, Standard unless
	// WithResolution says otherwise. Datadog ignores it.
	Resolution Resolution
	// Timestamp is the time of this data point alone, for providers that
	// timestamp each point (Datadog). The zero time means now. CloudWatch
	// EMF timestamps a whole document (SetTimestamp) and ignores it.
	Timestamp time.Time
	// Tags qualify this data point alone, for providers that tag each point
	// (Datadog). CloudWatch EMF dimensions apply to a whole document
	// (AddDimension) and ignore them.
	Tags []Dimension
}

// MetricOption configures one data point added with AddMetric.
type MetricOption func(*Metric)

// WithResolution sets the storage resolution of the metric: High for
// one-second data points, Standard (the default) for one-minute ones.
func WithResolution(r Resolution) MetricOption {
	return func(m *Metric) { m.Resolution = r }
}

// newMetric builds the data point of an AddMetric call.
func newMetric(name string, unit Unit, value float64, opts []MetricOption) Metric {
	m := Metric{Name: name, Unit: unit, Value: value, Resolution: Standard}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Provider is a metrics backend: CloudWatch EMF, the default, or another one
// such as Datadog (package metrics/datadog). It makes the metric sets that
// invocations fill and knows how to record a cold start; LogMetrics and the
// package functions do the rest, whatever the provider.
type Provider interface {
	// NewSet returns an empty metric set for one invocation, carrying the
	// default dimensions (default tags, for Datadog).
	NewSet(ctx context.Context, defaults []Dimension) Set
	// AddColdStartMetric records that the invocation set belongs to started
	// the execution environment; functionName names the function. CloudWatch
	// EMF writes a document of its own right away; Datadog adds a metric to
	// set.
	AddColdStartMetric(ctx context.Context, set Set, functionName string) error
}

// Set holds the metrics of one invocation until they are flushed. The
// metrics package calls a Set from one goroutine at a time, so an
// implementation needs no locking of its own. A method a backend has no use
// for returns an error wrapping errors.ErrUnsupported.
type Set interface {
	// AddMetric validates m and adds it. A set that reaches its backend's
	// limits writes what it holds and starts over, as Powertools does with
	// EMF documents of 100 metrics.
	AddMetric(ctx context.Context, m Metric) error
	// AddDimension adds a dimension to every metric of the set.
	AddDimension(ctx context.Context, name, value string) error
	// AddDimensions adds a dimension set: another combination of dimensions
	// the metrics are aggregated by.
	AddDimensions(ctx context.Context, dims []Dimension) error
	// AddMetadata adds a value written next to the metrics, searchable in
	// the logs but not a dimension.
	AddMetadata(ctx context.Context, key string, value any) error
	// SetTimestamp sets the time of every metric of the set.
	SetTimestamp(ctx context.Context, t time.Time) error
	// Flush writes the metrics out and empties the set. An empty set warns,
	// or fails with ErrNoMetrics when raiseOnEmpty is true.
	Flush(ctx context.Context, raiseOnEmpty bool) error
	// Clear empties the set without writing it, keeping the defaults.
	Clear()
	// Len returns the number of metrics the set holds.
	Len() int
}
