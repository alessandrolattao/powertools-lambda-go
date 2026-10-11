package logging

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// InjectOption configures InjectContext.
type InjectOption func(*injectConfig)

type injectConfig struct {
	logEvent        *bool
	correlationPath string
	clearState      bool
	flushOnUncaught bool
}

// WithLogEvent logs every event the handler receives, at INFO, as the
// record's message. It defaults to POWERTOOLS_LOGGER_LOG_EVENT, which is off:
// events often carry personal data.
func WithLogEvent(enabled bool) InjectOption {
	return func(c *injectConfig) { c.logEvent = &enabled }
}

// WithCorrelationIDPath sets the correlation id of every invocation to what
// the JMESPath expression selects in the event; the Correlation constants
// cover the common event sources. The Powertools JMESPath functions are
// available. An expression that does not compile fails every invocation, as
// in Powertools Python: it is a programming error. An event the expression
// cannot be evaluated against (a payload that is not JSON the way the
// expression expects) leaves the invocation without a correlation id: a
// logging feature never stops a function, and the handler's own decoding
// reports a malformed event better.
func WithCorrelationIDPath(expression string) InjectOption {
	return func(c *injectConfig) { c.correlationPath = expression }
}

// WithClearState starts every invocation without the keys a middleware
// wrapping this one put in its context (AppendKeys, WithKeys, the
// correlation id): Powertools Python's clear_state. Keys appended during an
// invocation never reach the next one, with or without it.
func WithClearState(enabled bool) InjectOption {
	return func(c *injectConfig) { c.clearState = enabled }
}

// WithFlushBufferOnUncaughtError flushes the invocation's log buffer when the
// handler returns an error or panics, so that the records leading to the
// failure are written.
func WithFlushBufferOnUncaughtError(enabled bool) InjectOption {
	return func(c *injectConfig) { c.flushOnUncaught = enabled }
}

// InjectContext wraps a Lambda handler with what the logger does around each
// invocation, Powertools Python's inject_lambda_context:
//
//   - it gives the invocation its own state in the context: the keys of
//     AppendKeys and the correlation id, the sampling decision (drawn anew
//     for every invocation when a sampling rate is set) and the log buffer;
//   - it sets the correlation id (WithCorrelationIDPath) and logs the event
//     (WithLogEvent);
//   - when the handler fails, it flushes the buffer
//     (WithFlushBufferOnUncaughtError); when it panics, it logs the panic
//     with its stack at ERROR (WithLogUncaughtPanics), then lets the panic
//     go on;
//   - it drops what is left in the buffer when the invocation ends.
//
// The Lambda context keys (function_request_id, cold_start, ...) come from
// the context of each record, with or without InjectContext. An error the
// handler returns is not logged: Lambda records it, and the handler knows
// best which errors deserve a record.
//
//	lambda.Start(logger.InjectContext(handle, logging.WithCorrelationIDPath(logging.CorrelationAPIGatewayHTTP)))
func (l *Logger) InjectContext[E, R any](next middleware.Handler[E, R], opts ...InjectOption) middleware.Handler[E, R] {
	inj := newInjector(l.handler, reflect.ValueOf(next).Pointer(), opts)
	return func(ctx context.Context, event E) (R, error) {
		ctx, done, err := inj.start(ctx, event)
		defer done()
		if err != nil {
			var zero R
			return zero, err
		}
		defer func() {
			if r := recover(); r != nil {
				inj.panicked(ctx, r)
			}
		}()
		result, err := next(ctx, event)
		if err != nil && inj.flushOnUncaught {
			inj.h.flushQuietly(ctx)
		}
		return result, err
	}
}

// injector is what InjectContext does around each invocation, the parts that
// do not depend on the handler's types.
type injector struct {
	h               *Handler
	logEvent        bool
	correlation     *jmespath.Expression
	compileErr      error
	clearState      bool
	flushOnUncaught bool
	pc              uintptr
}

func newInjector(h *Handler, handlerEntry uintptr, opts []InjectOption) *injector {
	cfg := injectConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	inj := &injector{
		h:               h,
		logEvent:        env.Bool(env.LoggerLogEvent, false),
		clearState:      cfg.clearState,
		flushOnUncaught: cfg.flushOnUncaught,
	}
	if handlerEntry != 0 {
		// The event record is located at the handler: a return address
		// inside it, as runtime.Callers gives them.
		inj.pc = handlerEntry + 1
	}
	if cfg.logEvent != nil {
		inj.logEvent = *cfg.logEvent
	}
	if cfg.correlationPath != "" {
		inj.correlation, inj.compileErr = jmespath.Compile(cfg.correlationPath)
	}
	return inj
}

// start opens the invocation: its state in the context, its correlation id,
// its event logged. done closes it.
func (inj *injector) start(ctx context.Context, event any) (_ context.Context, done func(), err error) {
	ctx, owner := inj.h.core.begin(ctx, inj.clearState)
	done = func() {}
	if owner {
		done = func() { inj.h.core.end(ctx) }
	}
	if inj.compileErr != nil {
		return ctx, done, fmt.Errorf("logging: correlation id: %w", inj.compileErr)
	}
	if inj.correlation != nil {
		selected, err := inj.correlation.Search(event)
		if err != nil {
			debug.Logger().DebugContext(ctx, "logging: no correlation id in this event", "err", err)
		} else {
			setCorrelation(ctx, correlationValue(selected))
		}
	}
	if inj.logEvent {
		inj.h.logEvent(ctx, event, inj.pc)
	}
	return ctx, done, nil
}

// panicked sees a panicking handler through: the panic logged, the buffer
// flushed, then the panic resumed for aws-lambda-go to report. It runs in
// the deferred function that recovered r.
func (inj *injector) panicked(ctx context.Context, r any) {
	inj.h.uncaught(ctx, panicked(r), inj.flushOnUncaught)
	panic(r)
}

// correlationValue keeps what a correlation path selects as the
// correlation_id key: a string as it is, nothing for nothing, and anything
// else as the JSON value it is.
func correlationValue(selected any) slog.Value {
	switch v := selected.(type) {
	case nil:
		return slog.Value{}
	case string:
		return slog.StringValue(v)
	default:
		return slog.AnyValue(v)
	}
}

// logEvent logs the event at INFO, as the record's message.
func (h *Handler) logEvent(ctx context.Context, event any, pc uintptr) {
	if !h.Enabled(ctx, slog.LevelInfo) {
		return
	}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "", pc)
	if err := h.handle(ctx, r, event); err != nil {
		debug.Logger().DebugContext(ctx, "logging: log the event", "err", err)
	}
}

// uncaught handles a panicking handler: the panic logged at ERROR when the
// logger logs them, the buffer flushed when asked.
func (h *Handler) uncaught(ctx context.Context, p *panicError, flush bool) {
	if h.core.logPanics {
		var pc uintptr
		if len(p.callers) > 0 {
			pc = p.callers[0]
		}
		r := slog.NewRecord(time.Now(), slog.LevelError, p.Error(), pc)
		r.AddAttrs(slog.Any(ErrorKey, p))
		if err := h.handle(ctx, r, nil); err != nil {
			debug.Logger().DebugContext(ctx, "logging: log the panic", "err", err)
		}
	}
	if flush {
		h.flushQuietly(ctx)
	}
}

// flushQuietly flushes the buffer on the way out of a failed invocation,
// where a failure to write has no one to be reported to.
func (h *Handler) flushQuietly(ctx context.Context) {
	if err := h.FlushBuffer(ctx); err != nil {
		debug.Logger().DebugContext(ctx, "logging: flush the buffer", "err", err)
	}
}
