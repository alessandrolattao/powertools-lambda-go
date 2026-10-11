package eventhandler

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime/debug"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	pdebug "github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// ServiceError is an error that is an HTTP response: the status and the
// message of the body {"statusCode": ..., "message": ...}. Return it from a
// typed operation, or pass it to Error, to answer with it: Python's
// ServiceError and its subclasses (BadRequestError, NotFoundError...), which
// the constructors below build.
type ServiceError struct {
	StatusCode int
	// Message is a string, or any value that encodes to JSON.
	Message any
}

func (e *ServiceError) Error() string {
	if s, ok := e.Message.(string); ok {
		return s
	}
	data, err := json.Marshal(e.Message)
	if err != nil {
		return fmt.Sprint(e.Message)
	}
	return string(data)
}

// Is reports whether target is a *ServiceError with the same status, so that
// errors.Is(err, ErrNotFound) matches every 404.
func (e *ServiceError) Is(target error) bool {
	t, ok := target.(*ServiceError)
	return ok && t.StatusCode == e.StatusCode
}

// NewServiceError returns a ServiceError with status and message.
func NewServiceError(status int, message any) *ServiceError {
	return &ServiceError{StatusCode: status, Message: message}
}

// BadRequest is a 400 error: Python's BadRequestError.
func BadRequest(message string) *ServiceError {
	return NewServiceError(http.StatusBadRequest, message)
}

// Unauthorized is a 401 error: Python's UnauthorizedError.
func Unauthorized(message string) *ServiceError {
	return NewServiceError(http.StatusUnauthorized, message)
}

// Forbidden is a 403 error: Python's ForbiddenError.
func Forbidden(message string) *ServiceError {
	return NewServiceError(http.StatusForbidden, message)
}

// NotFound is a 404 error: Python's NotFoundError. An empty message is
// "Not found", Python's default.
func NotFound(message string) *ServiceError {
	if message == "" {
		message = "Not found"
	}
	return NewServiceError(http.StatusNotFound, message)
}

// RequestTimeout is a 408 error: Python's RequestTimeoutError.
func RequestTimeout(message string) *ServiceError {
	return NewServiceError(http.StatusRequestTimeout, message)
}

// RequestEntityTooLarge is a 413 error: Python's
// RequestEntityTooLargeError.
func RequestEntityTooLarge(message string) *ServiceError {
	return NewServiceError(http.StatusRequestEntityTooLarge, message)
}

// InternalServerError is a 500 error: Python's InternalServerError.
func InternalServerError(message string) *ServiceError {
	return NewServiceError(http.StatusInternalServerError, message)
}

// ServiceUnavailable is a 503 error: Python's ServiceUnavailableError.
func ServiceUnavailable(message string) *ServiceError {
	return NewServiceError(http.StatusServiceUnavailable, message)
}

// Sentinels for errors.Is: each matches every *ServiceError with its
// status.
var (
	ErrBadRequest            = &ServiceError{StatusCode: http.StatusBadRequest}
	ErrUnauthorized          = &ServiceError{StatusCode: http.StatusUnauthorized}
	ErrForbidden             = &ServiceError{StatusCode: http.StatusForbidden}
	ErrNotFound              = &ServiceError{StatusCode: http.StatusNotFound}
	ErrRequestTimeout        = &ServiceError{StatusCode: http.StatusRequestTimeout}
	ErrRequestEntityTooLarge = &ServiceError{StatusCode: http.StatusRequestEntityTooLarge}
	ErrInternalServerError   = &ServiceError{StatusCode: http.StatusInternalServerError}
	ErrServiceUnavailable    = &ServiceError{StatusCode: http.StatusServiceUnavailable}
)

// ValidationErrorDetail is one failure of a request or a response to its
// schema. Only Loc and Type reach the client, as in Python, which hides the
// rest "for security reasons"; Msg and Input are for logs.
type ValidationErrorDetail struct {
	// Loc locates the failure: where ("path", "query", "header",
	// "cookie", "body", "response"), then the name, then the field path,
	// with array indexes as ints.
	Loc []any `json:"loc"`
	// Type is the error code, Pydantic's: "missing", "int_parsing",
	// "string_too_short", "json_invalid"...
	Type string `json:"type"`
	// Msg explains the failure.
	Msg string `json:"-"`
	// Input is the value that failed, when there is one.
	Input any `json:"-"`
}

// RequestValidationError reports a request that does not match the input of
// its typed operation. It answers 422 with
// {"statusCode":422,"detail":[{"loc":[...],"type":"..."}]} unless an error
// handler takes it.
type RequestValidationError struct {
	Errors []ValidationErrorDetail
	// Body is the request body.
	Body []byte
}

func (e *RequestValidationError) Error() string {
	return "request validation failed: " + describeDetails(e.Errors)
}

// ResponseValidationError reports a response of a typed operation that does
// not match its schema, when response validation is on. It answers
// StatusCode with the body of RequestValidationError.
type ResponseValidationError struct {
	Errors []ValidationErrorDetail
	// StatusCode is the status the route or the router chose.
	StatusCode int
	// Body is the response that failed.
	Body any
}

func (e *ResponseValidationError) Error() string {
	return "response validation failed: " + describeDetails(e.Errors)
}

func describeDetails(details []ValidationErrorDetail) string {
	parts := make([]string, 0, len(details))
	for _, d := range details {
		loc := make([]string, 0, len(d.Loc))
		for _, l := range d.Loc {
			loc = append(loc, fmt.Sprint(l))
		}
		part := strings.Join(loc, ".") + ": " + d.Type
		if d.Msg != "" {
			part += " (" + d.Msg + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// UnsupportedContentTypeError reports a request body whose content type the
// typed operation does not read. It answers 415 with
// {"statusCode":415,"detail":[{"loc":["body"],"type":"unsupported_content_type"}]}.
type UnsupportedContentTypeError struct {
	ContentType string
}

func (e *UnsupportedContentTypeError) Error() string {
	return "unsupported content type: " + e.ContentType
}

// ResponseSizeExceededError reports a response bigger than its event
// source allows (1 MB for ALB, see WithResponseSizeValidation).
type ResponseSizeExceededError struct {
	ActualSize int
	MaxSize    int
}

func (e *ResponseSizeExceededError) Error() string {
	return fmt.Sprintf("Serialized response size (%d bytes) exceeds the maximum allowed size (%d bytes)", e.ActualSize, e.MaxSize)
}

// errorHandler is a handler registered with OnError.
type errorHandler struct {
	typ reflect.Type
	// exact calls the handler with an error of type typ.
	exact func(w http.ResponseWriter, r *http.Request, err error) error
	// as calls the handler when errors.As finds a typ in err's tree.
	as func(w http.ResponseWriter, r *http.Request, err error) (bool, error)
}

var errorType = reflect.TypeFor[error]()

// OnError registers fn as the handler of the errors of type E: Python's
// exception_handler. When a typed operation, a middleware or a handler
// (through Error) fails with an error, the router looks for a handler:
//
//  1. a 404 *ServiceError goes to the HandleNotFound handler, if any;
//  2. walking the error tree from the outermost error (Unwrap, depth
//     first), the first error whose concrete type has a handler wins;
//  3. otherwise the handlers of interface types, in registration order, the
//     first one errors.As matches; a handler for error itself, Python's
//     handler for Exception, comes last.
//
// fn writes the response. If it returns an error instead, that error gets
// the built-in response (a *ServiceError, a validation error) or fails the
// invocation. Registering a type again replaces its handler.
func OnError[E error](r *Router, fn func(w http.ResponseWriter, r *http.Request, err E) error) {
	r.addErrorHandler(&errorHandler{
		typ: reflect.TypeFor[E](),
		exact: func(w http.ResponseWriter, req *http.Request, err error) error {
			e, ok := errors.AsType[E](err)
			if !ok {
				return err
			}
			return fn(w, req, e)
		},
		as: func(w http.ResponseWriter, req *http.Request, err error) (bool, error) {
			e, ok := errors.AsType[E](err)
			if !ok {
				return false, nil
			}
			return true, fn(w, req, e)
		},
	})
}

func (r *Router) addErrorHandler(h *errorHandler) {
	for i, existing := range r.handlers {
		if existing.typ == h.typ {
			r.handlers[i] = h
			return
		}
	}
	r.handlers = append(r.handlers, h)
}

// dispatch finds the error handler of err and runs it.
func (r *Router) dispatch(w http.ResponseWriter, req *http.Request, err error) (bool, error) {
	var found *errorHandler
	var target error
	walkErrors(err, func(e error) bool {
		t := reflect.TypeOf(e)
		for _, h := range r.handlers {
			if h.typ.Kind() != reflect.Interface && h.typ == t {
				found, target = h, e
				return false
			}
		}
		return true
	})
	if found != nil {
		return true, found.exact(w, req, target)
	}
	var catchAll *errorHandler
	for _, h := range r.handlers {
		if h.typ.Kind() != reflect.Interface {
			continue
		}
		if h.typ == errorType {
			catchAll = h
			continue
		}
		if ok, res := h.as(w, req, err); ok {
			return true, res
		}
	}
	if catchAll != nil {
		return true, catchAll.exact(w, req, err)
	}
	return false, nil
}

// walkErrors visits err and the errors it wraps, depth first, until visit
// returns false.
func walkErrors(err error, visit func(error) bool) bool {
	if err == nil {
		return true
	}
	if !visit(err) {
		return false
	}
	switch u := err.(type) { //nolint:errorlint // walking the tree by hand is the point
	case interface{ Unwrap() error }:
		return walkErrors(u.Unwrap(), visit)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if !walkErrors(e, visit) {
				return false
			}
		}
	}
	return true
}

// Error answers the request with err, as raising it does in Python: through
// the error handlers of the router serving r (see OnError), else with the
// built-in response of a *ServiceError, a *RequestValidationError, a
// *ResponseValidationError or an *UnsupportedContentTypeError. Any other
// error is unhandled: the Lambda invocation fails with it (in debug mode, the
// response is a 500 with the error and the stack), and a local server
// answers 500. The status and the body the handler had written are
// discarded; the headers the middlewares set stay.
//
// Middlewares and plain handlers call Error to fail a request the way a
// typed operation does by returning an error.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	reset(w)
	st := stateFrom(r.Context())
	if st == nil {
		if !writeBuiltinError(w, r, err) {
			unhandled(w, r, err, false)
		}
		return
	}
	st.router.handleError(w, r, err)
}

func reset(w http.ResponseWriter) {
	if buf, ok := w.(*lambdahttp.ResponseBuffer); ok {
		buf.Reset()
	}
}

func (r *Router) handleError(w http.ResponseWriter, req *http.Request, err error) {
	if svc, ok := errors.AsType[*ServiceError](err); ok && svc.StatusCode == http.StatusNotFound && r.notFound != nil {
		r.notFound.ServeHTTP(w, req)
		return
	}
	if handled, res := r.dispatch(w, req, err); handled {
		if res == nil {
			return
		}
		reset(w)
		err = res
	}
	if writeBuiltinError(w, req, err) {
		return
	}
	unhandled(w, req, err, r.debug)
}

type validationBody struct {
	StatusCode int                     `json:"statusCode"`
	Detail     []ValidationErrorDetail `json:"detail"`
}

type serviceBody struct {
	StatusCode int `json:"statusCode"`
	Message    any `json:"message"`
}

// writeBuiltinError writes the response Powertools gives the errors it
// knows, and reports whether err is one.
func writeBuiltinError(w http.ResponseWriter, r *http.Request, err error) bool {
	status, body, ok := builtinResponse(err)
	if !ok {
		return false
	}
	if werr := WriteJSON(w, r, status, body); werr != nil {
		pdebug.Logger().DebugContext(r.Context(), "event handler: writing an error response failed", "err", werr)
	}
	return true
}

func builtinResponse(err error) (status int, body any, known bool) {
	if e, ok := errors.AsType[*RequestValidationError](err); ok {
		return http.StatusUnprocessableEntity, validationBody{http.StatusUnprocessableEntity, publicDetails(e.Errors)}, true
	}
	if e, ok := errors.AsType[*ResponseValidationError](err); ok {
		return e.StatusCode, validationBody{e.StatusCode, publicDetails(e.Errors)}, true
	}
	if e, ok := errors.AsType[*UnsupportedContentTypeError](err); ok {
		detail := []ValidationErrorDetail{{Loc: []any{inBody}, Type: "unsupported_content_type", Msg: e.Error()}}
		return http.StatusUnsupportedMediaType, validationBody{http.StatusUnsupportedMediaType, publicDetails(detail)}, true
	}
	if e, ok := errors.AsType[*ServiceError](err); ok {
		return e.StatusCode, serviceBody{e.StatusCode, e.Message}, true
	}
	return 0, nil, false
}

// publicDetails keeps what the client may see of validation errors.
func publicDetails(details []ValidationErrorDetail) []ValidationErrorDetail {
	out := make([]ValidationErrorDetail, len(details))
	for i, d := range details {
		out[i] = ValidationErrorDetail{Loc: d.Loc, Type: d.Type}
	}
	return out
}

// unhandled deals with an error nobody turned into a response.
func unhandled(w http.ResponseWriter, r *http.Request, err error, debugMode bool) {
	pdebug.Logger().DebugContext(r.Context(), "event handler: unhandled error", "err", err)
	if debugMode {
		w.Header().Set("Content-Type", ContentTypeText)
		w.WriteHeader(http.StatusInternalServerError)
		if _, werr := fmt.Fprintf(w, "%+v\n\n%s", err, debug.Stack()); werr != nil {
			pdebug.Logger().DebugContext(r.Context(), "event handler: writing the debug response failed", "err", werr)
		}
		return
	}
	if inv := lambdahttp.FromContext(r.Context()); inv != nil {
		if inv.Err == nil {
			inv.Err = err
		}
		return
	}
	// Outside Lambda (a local server) there is no invocation to fail.
	body := serviceBody{http.StatusInternalServerError, "Internal Server Error"}
	if werr := WriteJSON(w, r, http.StatusInternalServerError, body); werr != nil {
		pdebug.Logger().DebugContext(r.Context(), "event handler: writing the error response failed", "err", werr)
	}
}
