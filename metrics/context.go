package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/metrics/internal/warn"
)

type ctxKey struct{}

// operation is a call on a metric set.
type operation = func(ctx context.Context, s Set) error

// invocation is the metric set a context carries, with the set of the
// enclosing context, if any, as parent.
type invocation struct {
	owner        *Metrics
	parent       *invocation
	raiseOnEmpty bool

	// mu serializes the goroutines an invocation may fan out to, all
	// recording metrics with the same context.
	mu  sync.Mutex
	set Set
}

func fromContext(ctx context.Context) *invocation {
	inv, ok := ctx.Value(ctxKey{}).(*invocation)
	if !ok {
		return nil
	}
	return inv
}

// find returns the innermost set of m that ctx carries.
func (m *Metrics) find(ctx context.Context) *invocation {
	for inv := fromContext(ctx); inv != nil; inv = inv.parent {
		if inv.owner == m {
			return inv
		}
	}
	return nil
}

func (inv *invocation) do(ctx context.Context, op operation) error {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return op(ctx, inv.set)
}

func (inv *invocation) clear() {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.set.Clear()
}

// pending is the metric set of a Metrics for the calls of its methods made
// with a context that carries none of its sets: at package initialization,
// or from a goroutine that lost the invocation's context. It is flushed with
// the next invocation of that Metrics, or as soon as it fills up.
type pending struct {
	mu  sync.Mutex
	set Set
}

func (m *Metrics) pendingDo(ctx context.Context, op operation) error {
	m.pending.mu.Lock()
	defer m.pending.mu.Unlock()
	if m.pending.set == nil {
		m.pending.set = m.provider.NewSet(ctx, m.defaults)
	}
	if err := op(ctx, m.pending.set); err != nil {
		return err
	}
	if m.pending.set.Len() >= MaxMetrics {
		return m.pending.set.Flush(ctx, false)
	}
	return nil
}

// orphans are the calls of the package functions made with a context that
// carries no metric set. No provider is known for them, so they are kept as
// calls and replayed into a set of the first Metrics that flushes, which
// writes them in a document of their own. At most MaxMetrics metrics, and as
// many other calls, wait; beyond that the functions fail with ErrNoMetricSet
// rather than letting memory grow in a process that never flushes.
var orphans struct {
	mu     sync.Mutex
	ops    []operation
	points int // the AddMetric calls among ops
}

func addOrphan(op operation, points int) error {
	orphans.mu.Lock()
	defer orphans.mu.Unlock()
	if orphans.points+points > MaxMetrics || len(orphans.ops)-orphans.points+1-points > MaxMetrics {
		return fmt.Errorf("%w: %d calls already wait for the next flush", ErrNoMetricSet, len(orphans.ops))
	}
	orphans.ops = append(orphans.ops, op)
	orphans.points += points
	return nil
}

// takeOrphans returns the waiting calls when there is a metric among them,
// and forgets them. Dimensions and metadata alone keep waiting for metrics.
func takeOrphans() []operation {
	orphans.mu.Lock()
	defer orphans.mu.Unlock()
	if orphans.points == 0 {
		return nil
	}
	ops := orphans.ops
	orphans.ops, orphans.points = nil, 0
	return ops
}

func clearOrphans() {
	orphans.mu.Lock()
	defer orphans.mu.Unlock()
	orphans.ops, orphans.points = nil, 0
}

// flushPending writes the metrics of m's pending set and the orphan ones,
// each in documents of their own, and reports whether there were any.
func (m *Metrics) flushPending(ctx context.Context) (bool, error) {
	var errs []error
	flushed := false
	m.pending.mu.Lock()
	if s := m.pending.set; s != nil && s.Len() > 0 {
		flushed = true
		errs = append(errs, s.Flush(ctx, false))
	}
	m.pending.mu.Unlock()
	if ops := takeOrphans(); ops != nil {
		flushed = true
		s := m.provider.NewSet(ctx, m.defaults)
		for _, op := range ops {
			errs = append(errs, op(ctx, s))
		}
		if s.Len() > 0 {
			errs = append(errs, s.Flush(ctx, false))
		}
	}
	return flushed, errors.Join(errs...)
}

// checkValue rejects what no backend can write: NaN and infinities.
func checkValue(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%w: %v is not a valid number", ErrMetricValue, v)
	}
	return nil
}

// dispatch runs op on the innermost set ctx carries, or keeps it for the
// next flush.
func dispatch(ctx context.Context, points int, op operation) error {
	if inv := fromContext(ctx); inv != nil {
		return inv.do(ctx, op)
	}
	return addOrphan(op, points)
}

// dispatch runs op on the innermost set of m that ctx carries, or on m's
// pending set.
func (m *Metrics) dispatch(ctx context.Context, op operation) error {
	if inv := m.find(ctx); inv != nil {
		return inv.do(ctx, op)
	}
	return m.pendingDo(ctx, op)
}

// AddMetric adds a data point to the metric set ctx carries: the one
// LogMetrics or NewContext installed. A name added again collects one more
// value; the unit and resolution of the last call win.
//
// With CloudWatch EMF, the name is trimmed and must be 1 to 255 characters
// long (ErrMetricName), the unit must be one CloudWatch knows
// (ErrMetricUnit), the resolution High or Standard (ErrMetricResolution),
// and the value finite (ErrMetricValue). The hundredth metric, or the
// hundredth value of one metric, writes the document out and starts a new
// one with the same dimensions and metadata.
//
// A context that carries no metric set keeps the metric until the next
// flush of any metric set, which writes it in a document of its own. The
// provider's checks run then; only the value is checked right away.
func AddMetric(ctx context.Context, name string, unit Unit, value float64, opts ...MetricOption) error {
	metric := newMetric(name, unit, value, opts)
	if err := checkValue(metric.Value); err != nil {
		return err
	}
	return dispatch(ctx, 1, func(ctx context.Context, s Set) error { return s.AddMetric(ctx, metric) })
}

// AddDimension adds a dimension to every metric of the set ctx carries. A
// name added again takes the new value, with a warning when it changes; an
// empty name or value is skipped with a warning; a dimension beyond
// MaxDimensions fails with ErrTooManyDimensions.
func AddDimension(ctx context.Context, name, value string) error {
	return dispatch(ctx, 0, func(ctx context.Context, s Set) error { return s.AddDimension(ctx, name, value) })
}

// AddDimensions adds a dimension set to the set ctx carries: one more
// combination of dimensions CloudWatch aggregates the metrics by, made of
// the default dimensions and dims. Their values join the document, the last
// value of a name winning.
func AddDimensions(ctx context.Context, dims ...Dimension) error {
	dims = append([]Dimension(nil), dims...)
	return dispatch(ctx, 0, func(ctx context.Context, s Set) error { return s.AddDimensions(ctx, dims) })
}

// AddMetadata adds a value to the document of the set ctx carries, next to
// the metrics: searchable in CloudWatch Logs Insights, not a dimension, so
// it does not multiply the metrics (a request id, a booking id). value is
// marshalled with encoding/json/v2 when it is added; ErrMetadata reports one
// that cannot be.
func AddMetadata(ctx context.Context, key string, value any) error {
	return dispatch(ctx, 0, func(ctx context.Context, s Set) error { return s.AddMetadata(ctx, key, value) })
}

// SetTimestamp sets the time of every metric of the set ctx carries, now by
// default. CloudWatch drops metrics more than 14 days in the past or 2 hours
// in the future; such a timestamp is kept, with a warning.
func SetTimestamp(ctx context.Context, t time.Time) error {
	return dispatch(ctx, 0, func(ctx context.Context, s Set) error { return s.SetTimestamp(ctx, t) })
}

// Flush writes the metrics of the set ctx carries and empties it, with the
// metrics that waited outside any set. LogMetrics flushes by itself; Flush
// is for the sets of NewContext. A context without a metric set gives
// ErrNoMetricSet.
func Flush(ctx context.Context) error {
	inv := fromContext(ctx)
	if inv == nil {
		return ErrNoMetricSet
	}
	return inv.owner.flush(ctx, inv, false)
}

// ClearMetrics empties the set ctx carries without writing it, keeping the
// default dimensions; without a set, it drops the metrics waiting for the
// next flush.
func ClearMetrics(ctx context.Context) {
	if inv := fromContext(ctx); inv != nil {
		inv.clear()
		return
	}
	clearOrphans()
}

// AddMetric adds a data point to m's set in ctx, like the AddMetric
// function. Without such a set, the metric waits in m's own pending set,
// flushed with m's next invocation, or as soon as it holds MaxMetrics
// metrics.
func (m *Metrics) AddMetric(ctx context.Context, name string, unit Unit, value float64, opts ...MetricOption) error {
	metric := newMetric(name, unit, value, opts)
	return m.dispatch(ctx, func(ctx context.Context, s Set) error { return s.AddMetric(ctx, metric) })
}

// AddDimension adds a dimension to m's set in ctx, like the AddDimension
// function, or to m's pending set.
func (m *Metrics) AddDimension(ctx context.Context, name, value string) error {
	return m.dispatch(ctx, func(ctx context.Context, s Set) error { return s.AddDimension(ctx, name, value) })
}

// AddDimensions adds a dimension set to m's set in ctx, like the
// AddDimensions function, or to m's pending set.
func (m *Metrics) AddDimensions(ctx context.Context, dims ...Dimension) error {
	return m.dispatch(ctx, func(ctx context.Context, s Set) error { return s.AddDimensions(ctx, dims) })
}

// AddMetadata adds metadata to m's set in ctx, like the AddMetadata
// function, or to m's pending set.
func (m *Metrics) AddMetadata(ctx context.Context, key string, value any) error {
	return m.dispatch(ctx, func(ctx context.Context, s Set) error { return s.AddMetadata(ctx, key, value) })
}

// SetTimestamp sets the timestamp of m's set in ctx, like the SetTimestamp
// function, or of m's pending set.
func (m *Metrics) SetTimestamp(ctx context.Context, t time.Time) error {
	return m.dispatch(ctx, func(ctx context.Context, s Set) error { return s.SetTimestamp(ctx, t) })
}

// Flush writes m's set in ctx, like the Flush function. Without such a set
// it writes m's pending set and the metrics that waited outside any set,
// warning when there are none.
func (m *Metrics) Flush(ctx context.Context) error {
	if inv := m.find(ctx); inv != nil {
		return m.flush(ctx, inv, false)
	}
	flushed, err := m.flushPending(ctx)
	if !flushed && err == nil {
		warn.Once(ctx, warn.UserWarning, warn.NoMetrics)
	}
	return err
}

// ClearMetrics empties m's set in ctx without writing it, or m's pending
// set.
func (m *Metrics) ClearMetrics(ctx context.Context) {
	if inv := m.find(ctx); inv != nil {
		inv.clear()
		return
	}
	m.pending.mu.Lock()
	defer m.pending.mu.Unlock()
	if m.pending.set != nil {
		m.pending.set.Clear()
	}
}
