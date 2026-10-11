package appsync

import (
	"errors"
	"strconv"
)

// The errors Resolve fails with are returned as they are, never wrapped:
// aws-lambda-go reports the name of the returned error's type as the
// errorType of the failed invocation, and AppSync shows it to the client
// next to the message. Their names are therefore part of the contract.

// ErrNoEvent is returned by DecodeEvent when the context does not come from
// Router.Resolve.
var ErrNoEvent = errors.New("appsync: no AppSync resolver event in the context")

// ResolverNotFoundError reports an invocation for a type and field that no
// resolver is registered for (Powertools Python raises ValueError for a
// single invocation and ResolverNotFoundError for a batch). Batch tells
// which registry was searched: a single resolver does not resolve a batch,
// nor a batch resolver a single event.
type ResolverNotFoundError struct {
	TypeName  string
	FieldName string
	Batch     bool
}

// Error returns the message Powertools Python uses.
func (e *ResolverNotFoundError) Error() string {
	return "No resolver found for '" + e.TypeName + "." + e.FieldName + "'"
}

// InvalidBatchResponseError reports a batch resolver whose results do not
// match its batch one to one: AppSync pairs results and requests by
// position, so a result missing or in excess would answer the wrong field.
type InvalidBatchResponseError struct {
	TypeName  string
	FieldName string
	// Items is the size of the batch, Results the number of results the
	// resolver returned.
	Items, Results int
}

// Error describes the mismatch.
func (e *InvalidBatchResponseError) Error() string {
	return "The response must be a List with one result per batch item when using batch resolvers: " +
		"the resolver of '" + e.TypeName + "." + e.FieldName + "' returned " +
		strconv.Itoa(e.Results) + " results for " + strconv.Itoa(e.Items) + " items"
}

// ArgumentsError reports GraphQL arguments that do not decode into the
// arguments type of the resolver.
type ArgumentsError struct {
	TypeName  string
	FieldName string
	Err       error
}

// Error describes the arguments that failed.
func (e *ArgumentsError) Error() string {
	return "invalid arguments for '" + e.TypeName + "." + e.FieldName + "': " + e.Err.Error()
}

// Unwrap returns the decoding error.
func (e *ArgumentsError) Unwrap() error { return e.Err }

// InvalidEventError reports an invocation payload that is not an AppSync
// resolver event: neither an object (a single invocation) nor an array of
// objects (a batch).
type InvalidEventError struct {
	Err error
}

// Error describes the payload problem.
func (e *InvalidEventError) Error() string {
	return "invalid AppSync resolver event: " + e.Err.Error()
}

// Unwrap returns the decoding error.
func (e *InvalidEventError) Unwrap() error { return e.Err }
