package eventhandler

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/swaggest/openapi-go"
)

// Register registers a typed operation for pattern, which must name a
// method ("GET /todos/{id}"): the handler receives its input bound from the
// request into In and returns the Out written as the response, the Go side
// of Python's routes with data validation.
//
// In is a struct whose fields say where their values come from:
//
//	type GetTodo struct {
//		ID      int    `path:"id" minimum:"1"`
//		Fields  string `query:"fields" enum:"all,title"`
//		TraceID string `header:"X-Trace-Id"`
//		Session string `cookie:"session"`
//	}
//
// A field named Body is the request body, decoded from JSON (a []byte Body
// takes the raw body of any content type). Fields tagged formData:"name"
// are form fields of an application/x-www-form-urlencoded or
// multipart/form-data body; a *multipart.FileHeader (or a slice of them) is
// an uploaded file. Embedded structs contribute their fields. Fields
// without one of these tags or names are left alone.
//
// Values are converted to the field's type: strings, booleans, integers,
// floats, types implementing encoding.TextUnmarshaler (time.Time), pointers
// to them (nil when absent) and slices of them (every value of a repeated
// parameter; header values split on commas). Constraints use the tags of
// github.com/swaggest/jsonschema-go: required, minimum, maximum,
// exclusiveMinimum, exclusiveMaximum, multipleOf, minLength, maxLength,
// pattern, enum, format, minItems, maxItems, default, description, example,
// deprecated. Path parameters are always required.
//
// The same schemas validate the request, describe the operation in the
// OpenAPI document and, when enabled, validate the response, so the three
// cannot drift. A request that does not conform answers 422 with Python's
// body; a body of a content type the operation does not read answers 415.
//
// Out is written as JSON with the route's status (200 unless StatusCode
// says otherwise), or as a Response when the operation picks its own status,
// headers and cookies. An error from op is answered through the error
// handlers (see OnError); an error none handles fails the invocation, as an
// exception does in Python.
//
// Register panics when the pattern is invalid or has no method, when In is
// not a struct or binds a field of a type it cannot convert, or when the
// path parameters of the pattern and of In differ.
func Register[In, Out any](r *Router, pattern string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	p, err := parsePattern(pattern)
	if err != nil {
		panic(err)
	}
	if p.method == "" {
		panic(fmt.Errorf("%w %q: a typed operation needs a method", ErrInvalidPattern, pattern))
	}
	registerOperation(r, p, op, opts)
}

// RegisterRegexp registers a typed operation for a regular expression
// route: see Register and Router.HandleRegexp. The named groups of re are
// the path parameters.
func RegisterRegexp[In, Out any](r *Router, method string, re *regexp.Regexp, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	if method == "" {
		panic(fmt.Errorf("%w %q: a typed operation needs a method", ErrInvalidPattern, re.String()))
	}
	p, err := regexpPattern(method, re)
	if err != nil {
		panic(err)
	}
	registerOperation(r, p, op, opts)
}

func registerOperation[In, Out any](r *Router, p *pattern, op func(context.Context, In) (Out, error), opts []RouteOption) {
	o, err := newOperation(p, op)
	if err != nil {
		panic(err)
	}
	r.add(&route{pat: p, handler: o.handler(), cfg: newRouteConfig(opts), op: o, funcName: funcName(op)})
}

// Get registers a typed GET operation for path; see Register.
func Get[In, Out any](r *Router, path string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	Register(r, http.MethodGet+" "+path, op, opts...)
}

// Post registers a typed POST operation for path; see Register.
func Post[In, Out any](r *Router, path string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	Register(r, http.MethodPost+" "+path, op, opts...)
}

// Put registers a typed PUT operation for path; see Register.
func Put[In, Out any](r *Router, path string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	Register(r, http.MethodPut+" "+path, op, opts...)
}

// Patch registers a typed PATCH operation for path; see Register.
func Patch[In, Out any](r *Router, path string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	Register(r, http.MethodPatch+" "+path, op, opts...)
}

// Delete registers a typed DELETE operation for path; see Register.
func Delete[In, Out any](r *Router, path string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	Register(r, http.MethodDelete+" "+path, op, opts...)
}

// Head registers a typed HEAD operation for path; see Register.
func Head[In, Out any](r *Router, path string, op func(ctx context.Context, in In) (Out, error), opts ...RouteOption) {
	Register(r, http.MethodHead+" "+path, op, opts...)
}

// operationDoc is what the router needs from a typed operation, whatever its
// types.
type operationDoc interface {
	handler() http.Handler
	describe(oc openapi.OperationContext, rt *route, responseValidation int)
	bodyRequired() bool
	withPattern(p *pattern) operationDoc
}

type operation[In, Out any] struct {
	pat  *pattern
	fn   func(context.Context, In) (Out, error)
	plan *inputPlan
	// out is the type of the response body: Out, or T for a Response[T].
	out        reflect.Type
	validators func() (*validators, error)
}

func newOperation[In, Out any](p *pattern, fn func(context.Context, In) (Out, error)) (*operation[In, Out], error) {
	plan, err := planInput(reflect.TypeFor[In]())
	if err == nil {
		err = plan.checkPath(p.names())
	}
	if err != nil {
		return nil, fmt.Errorf("route %q: %w", p.str, err)
	}
	o := &operation[In, Out]{pat: p, fn: fn, plan: plan, out: responseBodyType(reflect.TypeFor[Out]())}
	o.validators = sync.OnceValues(o.compile)
	return o, nil
}

// responseBodyType returns the type the body of an Out is.
func responseBodyType(t reflect.Type) reflect.Type {
	base := t
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if base.Kind() != reflect.Interface && base.Implements(reflect.TypeFor[responder]()) {
		if r, ok := reflect.TypeAssert[responder](reflect.Zero(base)); ok {
			return r.bodyType()
		}
	}
	return t
}

func (o *operation[In, Out]) handler() http.Handler { return o }

func (o *operation[In, Out]) bodyRequired() bool {
	if o.plan.body != nil {
		return o.plan.body.required
	}
	for _, p := range o.plan.params {
		if p.in == inForm && p.required {
			return true
		}
	}
	return false
}

func (o *operation[In, Out]) withPattern(p *pattern) operationDoc {
	cp, err := newOperation(p, o.fn)
	if err != nil {
		panic(err)
	}
	return cp
}

// describe adds the input and the responses of the operation.
func (o *operation[In, Out]) describe(oc openapi.OperationContext, rt *route, responseValidation int) {
	o.describeInput(oc)
	if len(rt.cfg.responses) > 0 {
		for _, doc := range rt.cfg.responses {
			model := doc.model
			if model == nil {
				model = o.outModel()
			}
			addResponse(oc, model, doc.status, doc.description)
		}
	} else {
		addResponse(oc, o.outModel(), rt.cfg.status, successDescription(rt))
	}
	describeValidationResponses(oc, responseValidation)
}

func (o *operation[In, Out]) describeInput(oc openapi.OperationContext) {
	oc.AddReqStructure(reflect.New(o.plan.typ).Interface(), openapi.WithContentType(formContentType))
	body := o.plan.body
	switch {
	case body == nil:
	case body.raw:
		oc.AddReqStructure(nil, func(cu *openapi.ContentUnit) {
			cu.ContentType, cu.Format = "application/octet-stream", "binary"
		})
	default:
		oc.AddReqStructure(reflect.Zero(body.typ).Interface(), openapi.WithContentType(ContentTypeJSON))
	}
}

// outModel returns a value of the response body type for the reflector, nil
// when the body is not described.
func (o *operation[In, Out]) outModel() any {
	if o.out.Kind() == reflect.Interface {
		return nil
	}
	return reflect.Zero(o.out).Interface()
}

const (
	formContentType      = "application/x-www-form-urlencoded"
	multipartContentType = "multipart/form-data"
)

// ServeHTTP binds the input, runs the operation and writes its output.
func (o *operation[In, Out]) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	st := stateFrom(ctx)
	in, err := o.bind(ctx, req, st)
	if err != nil {
		Error(w, req, err)
		return
	}
	out, err := o.fn(ctx, in)
	if err != nil {
		Error(w, req, err)
		return
	}
	if err := o.write(w, req, st, out); err != nil {
		Error(w, req, err)
	}
}

// write sends out as the response.
func (o *operation[In, Out]) write(w http.ResponseWriter, req *http.Request, st *requestState, out Out) error {
	status := http.StatusOK
	if st != nil && st.route != nil {
		status = st.route.cfg.status
	}
	parts := unpack(out, status)
	if st != nil && parts.compress != nil {
		st.compress = parts.compress
	}
	if err := o.validateResponse(req, st, parts.body); err != nil {
		return err
	}
	for name, values := range parts.header {
		w.Header()[http.CanonicalHeaderKey(name)] = values
	}
	for _, c := range parts.cookies {
		http.SetCookie(w, c)
	}
	if !bodyAllowed(parts.status) {
		w.WriteHeader(parts.status)
		return nil
	}
	raw, ok := parts.body.([]byte)
	if !ok {
		return WriteJSON(w, req, parts.status, parts.body)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(parts.status)
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write the response: %w", err)
	}
	return nil
}

// unpack splits what an operation returned into the parts of the response.
func unpack(out any, status int) responseParts {
	parts := responseParts{status: status, body: out}
	if v := reflect.ValueOf(out); v.Kind() == reflect.Pointer && v.IsNil() {
		return parts
	}
	resp, ok := out.(responder)
	if !ok {
		return parts
	}
	p := resp.parts()
	if p.status == 0 {
		p.status = status
	}
	return p
}

func bodyAllowed(status int) bool {
	return status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
}

// checkPath verifies that In binds exactly the path parameters of the
// pattern.
func (p *inputPlan) checkPath(names []string) error {
	declared := map[string]bool{}
	for _, f := range p.params {
		if f.in == inPath {
			declared[f.name] = true
		}
	}
	var missing []string
	extra := make([]string, 0, len(declared))
	for _, n := range names {
		if !declared[n] {
			missing = append(missing, n)
		}
		delete(declared, n)
	}
	for n := range declared {
		extra = append(extra, n)
	}
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("%w: the input struct must bind every path parameter and only those (missing: %s; unknown: %s)",
			ErrInvalidOperation, strings.Join(missing, ","), strings.Join(extra, ","))
	}
	return nil
}
