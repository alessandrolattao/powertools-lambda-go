package appsyncevents

import (
	"errors"
	"reflect"

	"github.com/aws/aws-lambda-go/lambda/messages"
)

// unauthorizedType is the errorType AppSync Events reads to reject an
// operation, the name of Powertools Python's exception.
const unauthorizedType = "UnauthorizedException"

// UnauthorizedError rejects the operation, Powertools Python's
// UnauthorizedException: returned (or wrapped) by a subscribe handler, the
// subscription is refused with HTTP 403; by a publish handler, nothing is
// broadcast. In both cases the invocation fails with the errorType
// "UnauthorizedException" AppSync Events expects.
type UnauthorizedError struct {
	Message string
}

// Error returns the message, "Unauthorized" when there is none.
func (e *UnauthorizedError) Error() string {
	if e.Message == "" {
		return "Unauthorized"
	}
	return e.Message
}

// invokeError is the failure aws-lambda-go reports as it is, with the
// errorType AppSync Events expects: the Go type's name would not do.
func (e *UnauthorizedError) invokeError() messages.InvokeResponse_Error {
	return messages.InvokeResponse_Error{Type: unauthorizedType, Message: e.Error()}
}

// PayloadError reports the payload of a published event that does not
// decode into the payload type of the handler.
type PayloadError struct {
	ID  string
	Err error
}

// Error describes the event that failed.
func (e *PayloadError) Error() string {
	return "invalid payload for event " + e.ID + ": " + e.Err.Error()
}

// Unwrap returns the decoding error.
func (e *PayloadError) Unwrap() error { return e.Err }

// InvalidEventError reports an invocation payload that is not an AppSync
// Events event.
type InvalidEventError struct {
	Err error
}

// Error describes the payload problem.
func (e *InvalidEventError) Error() string {
	return "invalid AppSync Events event: " + e.Err.Error()
}

// Unwrap returns the decoding error.
func (e *InvalidEventError) Unwrap() error { return e.Err }

// errNotObject is the cause of an InvalidEventError for a payload that is
// not a JSON object.
var errNotObject = errors.New("the payload is not an object")

// describe formats err the way Powertools Python reports a failed event to
// AppSync, "<name of the error's type> - <message>". The name is the one
// aws-lambda-go would report as the errorType: the Go type of err (of what
// it points to), or the Type of a messages.InvokeResponse_Error.
func describe(err error) string {
	if reflect.TypeOf(err) == reflect.TypeFor[messages.InvokeResponse_Error]() {
		if invokeErr, ok := errors.AsType[messages.InvokeResponse_Error](err); ok {
			return invokeErr.Type + " - " + invokeErr.Message
		}
	}
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		name = "Error"
	}
	return name + " - " + err.Error()
}
