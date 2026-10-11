package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SingleMetric is one metric in an EMF document of its own, with dimensions,
// metadata and a timestamp of its own: for a metric whose dimensions differ
// from those of the invocation's metrics, or one that happened at another
// time. It is Powertools Python's single_metric context manager; Flush is the
// end of the with block.
//
// The methods chain. The first error, from any of them, stops the others and
// is what Flush returns:
//
//	err := m.SingleMetric("RecordsCount", metrics.Count, 10).
//		AddDimension("TableName", "Users").
//		Flush()
type SingleMetric struct {
	m   *Metrics
	set *emfSet
	err error
}

// SingleMetric starts a document holding one metric, with m's namespace and
// service, but none of m's default dimensions unless AddDefaultDimensions
// adds them. The metric is validated as AddMetric validates it. Single
// metrics are a CloudWatch EMF feature: with another provider, Flush returns
// an error wrapping errors.ErrUnsupported.
func (m *Metrics) SingleMetric(name string, unit Unit, value float64, opts ...MetricOption) *SingleMetric {
	s := &SingleMetric{m: m}
	if m.emf == nil {
		s.err = fmt.Errorf("metrics: single metrics are CloudWatch EMF documents: %w", errors.ErrUnsupported)
		return s
	}
	s.set = m.emf.newSet(context.Background(), nil)
	s.err = s.set.AddMetric(context.Background(), newMetric(name, unit, value, opts))
	return s
}

// AddDimension adds a dimension to the metric, as AddDimension does.
func (s *SingleMetric) AddDimension(name, value string) *SingleMetric {
	if s.err == nil {
		s.err = s.set.AddDimension(context.Background(), name, value)
	}
	return s
}

// AddDefaultDimensions adds the default dimensions of the Metrics the
// single metric comes from, the way Powertools Python's single_metric does
// when given default_dimensions=metrics.default_dimensions.
func (s *SingleMetric) AddDefaultDimensions() *SingleMetric {
	for _, d := range s.m.defaults {
		s.AddDimension(d.Name, d.Value)
	}
	return s
}

// AddMetadata adds metadata to the document, as AddMetadata does.
func (s *SingleMetric) AddMetadata(key string, value any) *SingleMetric {
	if s.err == nil {
		s.err = s.set.AddMetadata(context.Background(), key, value)
	}
	return s
}

// SetTimestamp sets the time of the metric, as SetTimestamp does.
func (s *SingleMetric) SetTimestamp(t time.Time) *SingleMetric {
	if s.err == nil {
		s.err = s.set.SetTimestamp(context.Background(), t)
	}
	return s
}

// Flush writes the document, unless metrics are off, and returns the first
// error of the chain. A SingleMetric is written once; flushing it again
// fails with ErrNoMetrics.
func (s *SingleMetric) Flush() error {
	if s.err != nil {
		return s.err
	}
	return s.set.Flush(context.Background(), true)
}
