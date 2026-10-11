package validation

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// ValidatorOption configures Validator.
type ValidatorOption func(*validatorConfig)

type validatorConfig struct {
	inbound  *stage
	outbound *stage
}

// Inbound validates the event against schema before the handler runs:
// Python's inbound_schema, with opts in place of inbound_formats,
// inbound_handlers and inbound_provider_options. WithEnvelope among opts is
// Python's envelope: it selects the part of the event to validate, and
// that part is what the handler receives. A nil schema validates nothing,
// so that Inbound(nil, WithEnvelope(...)) only unwraps the event.
func Inbound(schema any, opts ...Option) ValidatorOption {
	return func(c *validatorConfig) { c.inbound = newStage(schema, opts) }
}

// Outbound validates the handler's response against schema: Python's
// outbound_schema, with opts in place of outbound_formats, outbound_handlers
// and outbound_provider_options. The defaults of the schema are filled into
// the response, as Python does to a dict response. WithEnvelope among opts
// validates a part of the response only (for example "powertools_json(body)"
// for an API Gateway response); the response then keeps no defaults.
func Outbound(schema any, opts ...Option) ValidatorOption {
	return func(c *validatorConfig) { c.outbound = newStage(schema, opts) }
}

// Validator validates the events and the responses of handler against JSON
// Schemas: Powertools Python's @validator decorator.
//
// The returned handler takes the raw event. It unwraps the event with the
// envelope of Inbound, validates it against the inbound schema, fills in
// the schema defaults, and decodes the result into the handler's event type
// E: the handler receives the payload, as in Python. E can be a struct, a
// map[string]any, a slice for envelopes that select a list (SQS, Kinesis,
// CloudWatch Logs), or jsontext.Value. After the handler returns, the
// response is validated against the outbound schema.
//
// Schemas given as documents are compiled once, when Validator is called; a
// schema that fails to compile, for example because a remote reference is
// unreachable, is compiled again on the next invocation, which fails with
// the *InvalidSchemaFormatError until it compiles.
func Validator[E, R any](handler middleware.Handler[E, R], opts ...ValidatorOption) middleware.Handler[jsontext.Value, R] {
	var cfg validatorConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg.inbound.warm()
	cfg.outbound.warm()
	return func(ctx context.Context, event jsontext.Value) (R, error) {
		payload, err := inbound[E](ctx, cfg.inbound, event)
		if err != nil {
			var zero R
			return zero, err
		}
		result, err := handler(ctx, payload)
		if err != nil {
			return result, err
		}
		return outbound(ctx, cfg.outbound, result)
	}
}

func inbound[E any](ctx context.Context, st *stage, event jsontext.Value) (E, error) {
	var payload E
	if st == nil {
		if err := json.Unmarshal(event, &payload); err != nil {
			return payload, fmt.Errorf("validation: decode the event into %T: %w", payload, err)
		}
		return payload, nil
	}
	p, err := st.prepare(ctx)
	if err != nil {
		return payload, err
	}
	if p.schema != nil {
		debug.Logger().DebugContext(ctx, "Validating inbound event")
	}
	data, changed, err := p.schema.check(event, p.expr, st.opts.defaults)
	if err != nil {
		return payload, err
	}
	if p.expr == nil && !changed {
		// The event is the payload as it came: decode it as it is.
		if err := json.Unmarshal(event, &payload); err != nil {
			return payload, fmt.Errorf("validation: decode the event into %T: %w", payload, err)
		}
		return payload, nil
	}
	if err := convert(data, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func outbound[R any](ctx context.Context, st *stage, result R) (R, error) {
	if st == nil {
		return result, nil
	}
	p, err := st.prepare(ctx)
	if err != nil {
		return result, err
	}
	if p.schema != nil {
		debug.Logger().DebugContext(ctx, "Validating outbound event")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return result, fmt.Errorf("validation: marshal the response: %w", err)
	}
	data, changed, err := p.schema.check(jsontext.Value(raw), p.expr, st.opts.defaults)
	if err != nil {
		return result, err
	}
	if changed && p.expr == nil {
		// Decoding the completed document over the response adds the
		// defaults and leaves everything else as it was.
		if err := convert(data, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// stage is one direction of Validator: a schema and an envelope, compiled
// once and shared by every invocation.
type stage struct {
	source any
	opts   options

	mu    sync.Mutex
	ready atomic.Pointer[prepared]
}

type prepared struct {
	schema *Schema // nil when nothing is validated
	expr   *jmespath.Expression
}

func newStage(schema any, opts []Option) *stage {
	return &stage{source: schema, opts: newOptions(opts)}
}

// warm compiles the stage while the function initializes, when the CPU is
// not billed to an invocation. A failure is retried by the invocations.
func (st *stage) warm() {
	if st == nil {
		return
	}
	if _, err := st.prepare(context.Background()); err != nil {
		debug.Logger().Debug("schema not compiled, retrying on invocation", "err", err)
	}
}

func (st *stage) prepare(ctx context.Context) (*prepared, error) {
	if p := st.ready.Load(); p != nil {
		return p, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if p := st.ready.Load(); p != nil {
		return p, nil
	}
	var p prepared
	if st.source != nil {
		s, err := schemaFor(ctx, st.source, &st.opts)
		if err != nil {
			return nil, err
		}
		p.schema = s
	}
	if st.opts.envelope != "" {
		expr, err := st.opts.expression()
		if err != nil {
			return nil, err
		}
		p.expr = expr
	}
	st.ready.Store(&p)
	return &p, nil
}
