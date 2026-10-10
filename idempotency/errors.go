package idempotency

import (
	"errors"
	"fmt"
)

// Errors an idempotent function returns.
var (
	// ErrAlreadyInProgress reports an invocation with the same idempotency
	// key that has not finished yet. It is safe to retry later.
	ErrAlreadyInProgress = errors.New("execution already in progress")
	// ErrPayloadValidation reports a payload whose validated part differs
	// from the one stored with the record of the same idempotency key.
	ErrPayloadValidation = errors.New("payload does not match stored record for this event key")
	// ErrNoIdempotencyKey reports a payload without an idempotency key, when
	// WithKeyRequired is on.
	ErrNoIdempotencyKey = errors.New("no data found to create a hashed idempotency key")
	// ErrInconsistentState reports a record that changed between the reads
	// of one invocation more times than the retries absorb.
	ErrInconsistentState = errors.New("idempotency store returned inconsistent results")
	// ErrInvalidStatus reports a stored record with a status Powertools does
	// not write.
	ErrInvalidStatus = errors.New("invalid idempotency record status")
	// ErrPersistence is what every PersistenceError matches.
	ErrPersistence = errors.New("idempotency store failure")
	// ErrInvalidConfig reports options or store settings that cannot work.
	// Idempotent panics with it, stores' constructors return it.
	ErrInvalidConfig = errors.New("invalid idempotency configuration")
)

// Errors of the Store contract, for those who write a Store.
var (
	// ErrItemAlreadyExists is what PutRecord returns when a record holds the
	// key. Return an ItemAlreadyExistsError instead when the record is known.
	ErrItemAlreadyExists = errors.New("idempotency record already exists")
	// ErrItemNotFound is what GetRecord returns when no record has the key.
	ErrItemNotFound = errors.New("idempotency record not found")
	// ErrMalformedRecord reports stored data that is not an idempotency
	// record.
	ErrMalformedRecord = errors.New("malformed idempotency record")
)

// AlreadyInProgressError reports an invocation with the same idempotency key
// that has not finished yet. It matches ErrAlreadyInProgress.
type AlreadyInProgressError struct {
	// Key is the idempotency key.
	Key string
	// KeyExpression is the event key JMESPath expression, empty when the key
	// is the hash of the whole payload.
	KeyExpression string
}

func (e *AlreadyInProgressError) Error() string {
	return fmt.Sprintf("execution already in progress with idempotency key: %s=%s", e.KeyExpression, e.Key)
}

// Is makes every AlreadyInProgressError match ErrAlreadyInProgress.
func (*AlreadyInProgressError) Is(target error) bool { return target == ErrAlreadyInProgress }

// The operations a PersistenceError reports.
const (
	OpPut    = "put"
	OpGet    = "get"
	OpUpdate = "update"
	OpDelete = "delete"
)

// PersistenceError reports a store operation that failed. The function's own
// errors are never wrapped in it, except when deleting the in-progress record
// after the function failed fails too: then both are joined. It matches
// ErrPersistence.
type PersistenceError struct {
	// Op is OpPut, OpGet, OpUpdate or OpDelete.
	Op string
	// Err is the cause.
	Err error
}

// Error words the failure as Powertools Python does.
func (e *PersistenceError) Error() string {
	var action string
	switch e.Op {
	case OpPut:
		action = "save in progress record to"
	case OpGet:
		action = "get record from"
	case OpUpdate:
		action = "update record state to success in"
	case OpDelete:
		action = "delete record from"
	default:
		action = e.Op + " record in"
	}
	return fmt.Sprintf("failed to %s idempotency store: %v", action, e.Err)
}

// Unwrap returns the cause.
func (e *PersistenceError) Unwrap() error { return e.Err }

// Is makes every PersistenceError match ErrPersistence.
func (*PersistenceError) Is(target error) bool { return target == ErrPersistence }

// InvalidStatusError reports a stored record with a status Powertools does
// not write. It matches ErrInvalidStatus.
type InvalidStatusError struct {
	Status Status
}

func (e *InvalidStatusError) Error() string {
	return fmt.Sprintf("invalid idempotency record status %q", string(e.Status))
}

// Is makes every InvalidStatusError match ErrInvalidStatus.
func (*InvalidStatusError) Is(target error) bool { return target == ErrInvalidStatus }

// ItemAlreadyExistsError is what PutRecord returns when a record holds the
// key and the store knows it without another read (DynamoDB returns it with
// the failed conditional write): carrying it saves a GetRecord. It matches
// ErrItemAlreadyExists.
type ItemAlreadyExistsError struct {
	// Old is the record that holds the key, nil when unknown.
	Old *Record
}

func (e *ItemAlreadyExistsError) Error() string {
	if e.Old == nil {
		return ErrItemAlreadyExists.Error()
	}
	return fmt.Sprintf("%v: key %q, status %s", ErrItemAlreadyExists, e.Old.Key, e.Old.Status)
}

// Is makes every ItemAlreadyExistsError match ErrItemAlreadyExists.
func (*ItemAlreadyExistsError) Is(target error) bool { return target == ErrItemAlreadyExists }
