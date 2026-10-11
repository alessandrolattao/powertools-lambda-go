package parser

import (
	"context"
	"encoding/json/jsontext"
	"reflect"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/middleware"
)

// EventParser turns a handler of a typed model into a handler of raw events,
// Python's @event_parser: each invocation parses the event into a T (through
// the envelope given with WithEnvelope, if any), validates it, then calls
// handler with it. A parsing failure is returned as a *ParseError and the
// handler does not run.
//
//	lambda.Start(parser.EventParser(handle, parser.WithEnvelope(parser.SQSEnvelope)))
//
// where handle is func(context.Context, []Order) (R, error). The model type
// is the handler's event type: Go infers it, as Python reads the type hint.
func EventParser[T, R any](handler middleware.Handler[T, R], opts ...Option) middleware.Handler[jsontext.Value, R] {
	model := reflect.TypeFor[T]().String()
	enveloped := newConfig(opts).envelope != nil
	return func(ctx context.Context, event jsontext.Value) (R, error) {
		debug.Logger().DebugContext(ctx, "parsing and validating the event", "model", model, "envelope", enveloped)
		parsed, err := Parse[T](event, opts...)
		if err != nil {
			var zero R
			return zero, err
		}
		return handler(ctx, parsed)
	}
}
