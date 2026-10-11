package schemavalidation

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/validation"
)

// Middleware returns a middleware that validates the JSON body of
// every request against the inbound JSON Schema and, when outbound is not
// nil, the body of every response against outbound: Python's
// SchemaValidationMiddleware, on top of the validation package. Schemas are
// what validation.Compile takes (a *validation.Schema, raw JSON, a
// map[string]any); opts are the validation options (custom formats,
// loaders...).
//
// A request that breaks the schema answers 400 "Bad Request: <message>", a
// response that does "Bad Response: <message>" (400, as in Python), and a
// schema that does not compile answers 500 "Internal Server Error". An empty
// body is validated as null, as Python validates a missing JSON body as
// None.
//
// Python validates the response only when outbound formats are given, and
// then against the inbound schema; this middleware validates it against the
// outbound schema whenever there is one.
func Middleware(inbound, outbound any, opts ...validation.Option) eventhandler.Middleware {
	v := &schemaValidator{}
	v.in, v.err = validation.Compile(inbound, opts...)
	if outbound != nil && v.err == nil {
		v.out, v.err = validation.Compile(outbound, opts...)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v.serve(next, w, r)
		})
	}
}

type schemaValidator struct {
	in, out *validation.Schema
	err     error
}

func (v *schemaValidator) serve(next http.Handler, w http.ResponseWriter, r *http.Request) {
	if v.err != nil {
		debug.Logger().DebugContext(r.Context(), "event handler: invalid schema", "err", v.err)
		eventhandler.Error(w, r, eventhandler.InternalServerError("Internal Server Error"))
		return
	}
	body, err := readAndRestore(r)
	if err != nil {
		eventhandler.Error(w, r, err)
		return
	}
	if err := validateBody(v.in, body); err != nil {
		eventhandler.Error(w, r, eventhandler.BadRequest("Bad Request: "+validationMessage(err)))
		return
	}
	if v.out == nil {
		next.ServeHTTP(w, r)
		return
	}
	rec := lambdahttp.NewResponseBuffer()
	next.ServeHTTP(rec, r)
	if err := validateBody(v.out, rec.Body()); err != nil {
		eventhandler.Error(w, r, eventhandler.BadRequest("Bad Response: "+validationMessage(err)))
		return
	}
	if err := rec.CopyTo(w); err != nil {
		debug.Logger().DebugContext(r.Context(), "event handler: writing the response failed", "err", err)
	}
}

func validateBody(schema *validation.Schema, body []byte) error {
	var event any = body
	if len(bytes.TrimSpace(body)) == 0 {
		event = nil
	}
	_, err := schema.Validate(event)
	return err
}

func validationMessage(err error) string {
	if ve, ok := errors.AsType[*validation.SchemaValidationError](err); ok {
		return ve.Message
	}
	return err.Error()
}

// readAndRestore reads the request body and puts it back for the handler.
func readAndRestore(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, eventhandler.BadRequest("Bad Request: unreadable body")
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}
