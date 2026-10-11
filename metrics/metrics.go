package metrics

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/internal/output"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// coldStartMetric names the metric LogMetrics records on a cold start.
const coldStartMetric = "ColdStart"

// Metrics creates metrics for a function: it decorates the handler with
// LogMetrics, which gives every invocation a metric set of its own and
// flushes it when the invocation ends. Create one per function, in main or at
// package level, and share it: it is safe for concurrent use.
type Metrics struct {
	provider     Provider
	emf          *emfProvider // the provider, when it is CloudWatch EMF
	functionName string
	defaults     []Dimension
	pending      pending
}

// config is what the options of New set.
type config struct {
	namespace    string
	service      string
	functionName string
	defaults     []Dimension
	out          io.Writer
	now          func() time.Time
	provider     Provider
}

// Option configures New.
type Option interface {
	applyMetrics(c *config)
}

// LogOption configures LogMetrics and NewContext.
type LogOption interface {
	applyLog(c *logConfig)
}

// DimensionsOption configures default dimensions; it is both an Option of
// New and a LogOption of LogMetrics and NewContext.
type DimensionsOption interface {
	Option
	LogOption
}

type optionFunc func(*config)

func (f optionFunc) applyMetrics(c *config) { f(c) }

type logOptionFunc func(*logConfig)

func (f logOptionFunc) applyLog(c *logConfig) { f(c) }

type defaultDimensions []Dimension

func (d defaultDimensions) applyMetrics(c *config) { c.defaults = mergeDimensions(c.defaults, d) }

func (d defaultDimensions) applyLog(c *logConfig) { c.defaults = mergeDimensions(c.defaults, d) }

// WithNamespace sets the CloudWatch namespace of the metrics, the container
// that groups the metrics of an application (ServerlessAirline). It defaults
// to POWERTOOLS_METRICS_NAMESPACE; flushing metrics without one fails with
// ErrNoNamespace.
func WithNamespace(namespace string) Option {
	return optionFunc(func(c *config) { c.namespace = namespace })
}

// WithService sets the value of the service dimension added to every metric
// set, and to the cold start metric. It defaults to POWERTOOLS_SERVICE_NAME;
// without either, metrics have no service dimension.
func WithService(service string) Option {
	return optionFunc(func(c *config) { c.service = service })
}

// WithFunctionName sets the function_name dimension of the cold start
// metric. It defaults to POWERTOOLS_METRICS_FUNCTION_NAME, then to the
// function's own name (AWS_LAMBDA_FUNCTION_NAME).
func WithFunctionName(name string) Option {
	return optionFunc(func(c *config) { c.functionName = name })
}

// WithDefaultDimensions adds dimensions to every metric set: given to New,
// to the sets of every invocation; given to LogMetrics or NewContext, to the
// sets of that handler or context, after the ones of New. A name given again
// keeps its place and takes the new value. Dimensions with an empty name or
// value are skipped with a warning.
func WithDefaultDimensions(dims ...Dimension) DimensionsOption {
	return defaultDimensions(slices.Clone(dims))
}

// WithoutDefaultDimensions drops the default dimensions given so far, the
// way Powertools Python's clear_default_dimensions does: given to
// LogMetrics, the handler's metric sets ignore those of New.
func WithoutDefaultDimensions() DimensionsOption {
	return clearDimensions{}
}

type clearDimensions struct{}

func (clearDimensions) applyMetrics(c *config) { c.defaults = nil }

func (clearDimensions) applyLog(c *logConfig) { c.defaults = nil }

// WithWriter sends the EMF documents to w instead of standard output. Each
// document reaches w in a single Write call, one at a time.
func WithWriter(w io.Writer) Option {
	return optionFunc(func(c *config) { c.out = w })
}

// WithClock sets the clock that timestamps documents, time.Now by default.
// Tests use it to make documents reproducible.
func WithClock(now func() time.Time) Option {
	return optionFunc(func(c *config) { c.now = now })
}

// WithProvider makes p the metrics backend instead of CloudWatch EMF. The
// options that configure EMF documents (WithNamespace, WithService,
// WithWriter, WithClock) are the provider's business then, and New ignores
// them. Package metrics/datadog returns a Metrics with its provider.
func WithProvider(p Provider) Option {
	return optionFunc(func(c *config) { c.provider = p })
}

// New returns a Metrics configured by opts, which writes CloudWatch EMF
// documents unless WithProvider names another backend.
//
// Metrics are off when POWERTOOLS_METRICS_DISABLED is true, or when
// POWERTOOLS_DEV is on and POWERTOOLS_METRICS_DISABLED is unset: the metric
// functions validate as usual, and nothing is written.
func New(opts ...Option) *Metrics {
	cfg := config{
		namespace:    env.Get(env.MetricsNamespace),
		service:      env.Get(env.ServiceName),
		functionName: env.Get(env.MetricsFunctionName),
		now:          time.Now,
	}
	for _, opt := range opts {
		opt.applyMetrics(&cfg)
	}
	m := &Metrics{
		provider:     cfg.provider,
		functionName: cfg.functionName,
		defaults:     cfg.defaults,
	}
	if m.provider == nil {
		m.emf = &emfProvider{
			namespace: cfg.namespace,
			service:   cfg.service,
			out:       output.For(cfg.out),
			now:       cfg.now,
			disabled:  env.MetricsOff(),
		}
		m.provider = m.emf
	}
	return m
}

// logConfig is what the options of LogMetrics and NewContext set.
type logConfig struct {
	coldStart    bool
	raiseOnEmpty bool
	defaults     []Dimension
}

// WithColdStartMetric makes LogMetrics record a ColdStart metric, of value
// 1, when the invocation is the first of its execution environment. With
// CloudWatch EMF the metric goes in a document of its own, with the
// function_name and service dimensions only, so that the dimensions of the
// application metrics do not multiply it. No metric is written for the other
// invocations, as zeros would cost without telling anything.
func WithColdStartMetric(enabled bool) LogOption {
	return logOptionFunc(func(c *logConfig) { c.coldStart = enabled })
}

// WithRaiseOnEmptyMetrics makes the flush at the end of an invocation fail
// with ErrNoMetrics when the invocation added no metric, instead of warning.
func WithRaiseOnEmptyMetrics(enabled bool) LogOption {
	return logOptionFunc(func(c *logConfig) { c.raiseOnEmpty = enabled })
}

func (m *Metrics) logConfig(opts []LogOption) logConfig {
	cfg := logConfig{defaults: slices.Clone(m.defaults)}
	for _, opt := range opts {
		opt.applyLog(&cfg)
	}
	return cfg
}

// LogMetrics decorates the handler next: every invocation gets a metric set
// of its own in its context, where AddMetric and the other functions record
// metrics, and the set is flushed when the handler returns, also when it
// returns an error. When the handler panics, the set is flushed and the
// panic goes on.
//
// A flush that fails (ErrNoMetrics with WithRaiseOnEmptyMetrics, a missing
// namespace) makes the invocation fail with that error, unless the handler
// failed first: its own error is kept then.
func (m *Metrics) LogMetrics[E, R any](next middleware.Handler[E, R], opts ...LogOption) middleware.Handler[E, R] {
	cfg := m.logConfig(opts)
	return func(ctx context.Context, event E) (result R, err error) {
		// Ask on entry, as every Powertools decorator does, so that all the
		// utilities agree on which invocation is the cold one.
		cold := coldstart.Is(ctx)
		ctx, inv := m.start(ctx, &cfg)
		defer func() {
			panicked := recover()
			flushErr := m.flush(ctx, inv, cold && cfg.coldStart)
			if panicked != nil || err != nil {
				if flushErr != nil {
					debug.Logger().DebugContext(ctx, "metrics flush failed after the handler failed", "err", flushErr)
				}
				if panicked != nil {
					panic(panicked)
				}
				return
			}
			err = flushErr
		}()
		return next(ctx, event)
	}
}

// NewContext returns a context carrying a new metric set of m, for code
// that flushes metrics itself instead of using LogMetrics (a handler behind
// the AWS Lambda Web Adapter, a middleware of its own): record metrics with
// ctx, then call Flush(ctx), also when the work fails. NewContext never
// records a cold start; WithColdStartMetric is ignored.
func (m *Metrics) NewContext(ctx context.Context, opts ...LogOption) context.Context {
	cfg := m.logConfig(opts)
	ctx, _ = m.start(ctx, &cfg)
	return ctx
}

func (m *Metrics) start(ctx context.Context, cfg *logConfig) (context.Context, *invocation) {
	inv := &invocation{
		owner:        m,
		parent:       fromContext(ctx),
		raiseOnEmpty: cfg.raiseOnEmpty,
		set:          m.provider.NewSet(ctx, cfg.defaults),
	}
	return context.WithValue(ctx, ctxKey{}, inv), inv
}

// flush ends the life of inv's set: the cold start metric first, then the
// metrics that waited outside any invocation, then the set.
func (m *Metrics) flush(ctx context.Context, inv *invocation, coldStart bool) error {
	var errs []error
	if coldStart {
		errs = append(errs, inv.do(ctx, func(ctx context.Context, s Set) error {
			return m.provider.AddColdStartMetric(ctx, s, m.coldStartFunctionName())
		}))
	}
	_, pendingErr := m.flushPending(ctx)
	errs = append(errs, pendingErr, inv.do(ctx, func(ctx context.Context, s Set) error {
		return s.Flush(ctx, inv.raiseOnEmpty)
	}))
	return errors.Join(errs...)
}

// coldStartFunctionName resolves the function_name of the cold start
// metric: the option, POWERTOOLS_METRICS_FUNCTION_NAME, the function's name.
func (m *Metrics) coldStartFunctionName() string {
	if m.functionName != "" {
		return m.functionName
	}
	return env.Get(env.LambdaFunctionName)
}

// mergeDimensions returns base with add applied in order: a name already
// there keeps its place and takes the new value, a new name goes last.
func mergeDimensions(base, add []Dimension) []Dimension {
	out := slices.Clone(base)
	for _, d := range add {
		if i := slices.IndexFunc(out, func(o Dimension) bool { return o.Name == d.Name }); i >= 0 {
			out[i].Value = d.Value
			continue
		}
		out = append(out, d)
	}
	return out
}
