package logging

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// errMarshalPanic reports a value whose JSON encoding panicked.
var errMarshalPanic = errors.New("logging: encoding a value panicked")

// maxPooledBuffer is the largest buffer returned to the pool, so that one
// huge record does not keep its memory for the life of the process.
const maxPooledBuffer = 64 << 10

// encoder writes records as JSON, with encoding/json/v2.
type encoder struct {
	pool     sync.Pool
	textOpts jsontext.Options
	jsonOpts json.Options
}

type encodeState struct {
	buf bytes.Buffer
	enc *jsontext.Encoder
}

func newEncodeState() *encodeState {
	s := &encodeState{}
	s.enc = jsontext.NewEncoder(&s.buf)
	return s
}

// newEncoder sets up the JSON options: strings are written as they are,
// invalid UTF-8 included (mangled rather than failing the record), and maps
// in a stable order.
func newEncoder(indent *string, jsonOptions []json.Options) *encoder {
	text := []jsontext.Options{jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true)}
	pretty := ""
	if env.IsDev() {
		pretty = "    "
	}
	if indent != nil {
		pretty = *indent
	}
	if pretty != "" {
		text = append(text, jsontext.Multiline(true), jsontext.WithIndent(pretty))
	}
	e := &encoder{textOpts: json.JoinOptions(text...)}
	e.jsonOpts = json.JoinOptions(append([]json.Options{
		jsontext.AllowInvalidUTF8(true), json.Deterministic(true),
	}, jsonOptions...)...)
	e.pool.New = func() any { return newEncodeState() }
	return e
}

// encode returns r as one line of JSON, newline included.
func (h *Handler) encode(ctx context.Context, r slog.Record, message any) ([]byte, error) {
	return h.core.encoder.render(h.build(ctx, r, message))
}

func (e *encoder) render(rec *record) ([]byte, error) {
	s, ok := e.pool.Get().(*encodeState)
	if !ok {
		s = newEncodeState()
	}
	defer func() {
		if s.buf.Cap() <= maxPooledBuffer {
			e.pool.Put(s)
		}
	}()
	s.buf.Reset()
	s.enc.Reset(&s.buf, e.textOpts)
	w := &jsonWriter{enc: s.enc, encoder: e}
	w.object(rec)
	if w.err != nil {
		return nil, fmt.Errorf("logging: encode a record: %w", w.err)
	}
	return bytes.Clone(s.buf.Bytes()), nil
}

// jsonWriter writes one record, keeping the first error: once the encoder
// fails, the record is lost anyway.
type jsonWriter struct {
	enc     *jsontext.Encoder
	encoder *encoder
	err     error
}

func (w *jsonWriter) token(t jsontext.Token) {
	if w.err == nil {
		w.err = w.enc.WriteToken(t)
	}
}

func (w *jsonWriter) raw(v jsontext.Value) {
	if w.err == nil {
		w.err = w.enc.WriteValue(v)
	}
}

func (w *jsonWriter) object(rec *record) {
	w.token(jsontext.BeginObject)
	for _, f := range rec.fields {
		if f.unset || isNil(f.value) {
			continue
		}
		w.token(jsontext.String(f.key))
		w.value(f.value)
	}
	w.token(jsontext.EndObject)
}

// isNil reports a value Powertools leaves out: nil, as Python strips None.
func isNil(v slog.Value) bool {
	return v.Kind() == slog.KindAny && v.Any() == nil
}

// value writes v the way slog's JSONHandler does.
func (w *jsonWriter) value(v slog.Value) {
	switch v.Kind() {
	case slog.KindString:
		w.token(jsontext.String(v.String()))
	case slog.KindInt64:
		w.token(jsontext.Int(v.Int64()))
	case slog.KindUint64:
		w.token(jsontext.Uint(v.Uint64()))
	case slog.KindFloat64:
		w.token(jsontext.Float(v.Float64()))
	case slog.KindBool:
		w.token(jsontext.Bool(v.Bool()))
	case slog.KindDuration:
		w.token(jsontext.Int(int64(v.Duration())))
	case slog.KindTime:
		w.token(jsontext.String(v.Time().Format(time.RFC3339Nano)))
	case slog.KindGroup:
		w.token(jsontext.BeginObject)
		for _, a := range v.Group() {
			w.token(jsontext.String(a.Key))
			w.value(a.Value)
		}
		w.token(jsontext.EndObject)
	case slog.KindLogValuer:
		w.value(v.Resolve())
	default:
		w.anyValue(v.Any())
	}
}

func (w *jsonWriter) anyValue(value any) {
	switch v := value.(type) {
	case nil:
		w.token(jsontext.Null)
	case eventMessage:
		if raw, ok := v.value.([]byte); ok && jsontext.Value(raw).IsValid() {
			w.raw(raw)
			return
		}
		w.marshal(v.value)
	case json.Marshaler, json.MarshalerTo:
		w.marshal(v)
	case error:
		w.token(jsontext.String(errorText(v)))
	default:
		w.marshal(v)
	}
}

// marshal writes value as encoding/json/v2 encodes it; a value it cannot
// encode is written as its text, as Powertools Python's json_default does.
func (w *jsonWriter) marshal(value any) {
	raw, err := w.encoder.marshal(value)
	if err != nil {
		w.token(jsontext.String(fmt.Sprint(value)))
		return
	}
	w.raw(raw)
}

func (e *encoder) marshal(value any) (raw jsontext.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			raw, err = nil, fmt.Errorf("%w: %v", errMarshalPanic, r)
		}
	}()
	raw, err = json.Marshal(value, e.jsonOpts)
	return raw, err //nolint:wrapcheck // the caller only checks that encoding succeeded
}
