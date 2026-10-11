package metrics

import (
	"errors"
	"fmt"
)

// The limits CloudWatch EMF puts on a document, as Powertools enforces them.
const (
	// MaxMetrics is the number of metrics, and of values of one metric, an
	// EMF document holds. Adding the hundredth writes the document out.
	MaxMetrics = 100
	// MaxDimensions is the number of dimensions a metric set holds, the
	// service dimension included. EMF accepts 30 per dimension set;
	// Powertools keeps one in reserve, as Python does.
	MaxDimensions = 29
	// MaxMetricNameLength is the longest metric name, in characters.
	MaxMetricNameLength = 255
)

// Errors returned by the metric functions. They match Powertools Python's
// exceptions: MetricNameError, MetricUnitError, MetricResolutionError,
// MetricValueError and SchemaValidationError.
var (
	// ErrMetricName reports a metric name CloudWatch (or Datadog) rejects.
	ErrMetricName = errors.New("metrics: invalid metric name")
	// ErrMetricUnit reports a unit CloudWatch does not know.
	ErrMetricUnit = errors.New("metrics: invalid metric unit")
	// ErrMetricResolution reports a storage resolution other than 1 or 60.
	ErrMetricResolution = errors.New("metrics: invalid metric resolution")
	// ErrMetricValue reports a value that is not a finite number.
	ErrMetricValue = errors.New("metrics: invalid metric value")
	// ErrSchemaValidation reports a metric set that would not make a valid
	// document. ErrNoMetrics, ErrNoNamespace and ErrTooManyDimensions wrap
	// it.
	ErrSchemaValidation = errors.New("metrics: schema validation failed")
	// ErrNoMetrics reports a flush without metrics when the decorator was
	// asked to fail on empty metric sets.
	ErrNoMetrics = fmt.Errorf("%w: must contain at least one metric", ErrSchemaValidation)
	// ErrNoNamespace reports a CloudWatch metric set flushed without a
	// namespace.
	ErrNoNamespace = fmt.Errorf("%w: must contain a metric namespace", ErrSchemaValidation)
	// ErrTooManyDimensions reports a dimension beyond MaxDimensions.
	ErrTooManyDimensions = fmt.Errorf("%w: maximum number of dimensions exceeded (%d)", ErrSchemaValidation, MaxDimensions)
	// ErrMetadata reports a metadata value that cannot be written as JSON.
	ErrMetadata = errors.New("metrics: invalid metadata value")
	// ErrNoMetricSet reports a flush with a context that carries no metric
	// set, or a context-less metric that finds the process-level pending set
	// full.
	ErrNoMetricSet = errors.New("metrics: no metric set in the context")
)
