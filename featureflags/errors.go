package featureflags

import (
	"errors"
	"fmt"
)

// The sentinel errors of the package, for errors.Is.
var (
	// ErrConfigurationStore: the store could not provide the configuration
	// (a network failure, a document that is not JSON). Evaluate and
	// EnabledFeatures absorb it and return the caller's default, as Python
	// does.
	ErrConfigurationStore = errors.New("unable to get the feature flags configuration")
	// ErrStoreClient: the store refused the request for a reason the caller
	// must hear about, such as missing permissions. It is never absorbed.
	ErrStoreClient = errors.New("feature flags store client error")
	// ErrSchemaValidation: the configuration is not a valid feature flags
	// schema. It is never absorbed.
	ErrSchemaValidation = errors.New("invalid feature flags schema")
	// ErrValueType: the value of a feature does not fit the Go type the
	// caller asked for.
	ErrValueType = errors.New("feature value does not fit the requested type")
	// ErrInvalidContext: the evaluation context holds a value that does not
	// marshal to JSON.
	ErrInvalidContext = errors.New("evaluation context is not JSON")
)

// The errors a condition can meet while comparing its operands, named after
// the Python exceptions they stand for. A ConditionError wraps one of them.
var (
	// ErrOperandType stands for Python's TypeError: operands of types the
	// action cannot compare, such as a string and a number.
	ErrOperandType = errors.New("operands have unsupported types")
	// ErrOperandNotString stands for Python's AttributeError: STARTSWITH or
	// ENDSWITH on a context value that is not a string.
	ErrOperandNotString = errors.New("operand is not a string")
	// ErrOperandNotList stands for Python's ValueError: ANY_IN_VALUE,
	// ALL_IN_VALUE or NONE_IN_VALUE on a context value that is not a list.
	ErrOperandNotList = errors.New("operand is not a list")
)

// ConfigurationStoreError reports a configuration the store could not
// provide: Python's ConfigurationStoreError.
type ConfigurationStoreError struct {
	Message string
	Err     error
}

func (e *ConfigurationStoreError) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *ConfigurationStoreError) Unwrap() error { return e.Err }

// Is makes the error match ErrConfigurationStore.
func (*ConfigurationStoreError) Is(target error) bool { return target == ErrConfigurationStore }

// StoreClientError reports a request the store refused, such as an access
// denied: Python's StoreClientError.
type StoreClientError struct {
	Err error
}

func (e *StoreClientError) Error() string {
	return fmt.Sprintf("feature flags store client error: %v", e.Err)
}

// Unwrap returns the cause.
func (e *StoreClientError) Unwrap() error { return e.Err }

// Is makes the error match ErrStoreClient.
func (*StoreClientError) Is(target error) bool { return target == ErrStoreClient }

// SchemaValidationError reports a configuration that is not a valid feature
// flags schema: Python's SchemaValidationError, with its messages. The
// messages name features and rules, never condition values, which may hold
// personal data.
type SchemaValidationError struct {
	Message string
}

func (e *SchemaValidationError) Error() string { return e.Message }

// Is makes the error match ErrSchemaValidation.
func (*SchemaValidationError) Is(target error) bool { return target == ErrSchemaValidation }

// ValueTypeError reports a feature value that does not fit the type the
// caller of Evaluate asked for.
type ValueTypeError struct {
	Feature string
	Err     error
}

func (e *ValueTypeError) Error() string {
	return fmt.Sprintf("feature %q: %v", e.Feature, e.Err)
}

// Unwrap returns the cause.
func (e *ValueTypeError) Unwrap() error { return e.Err }

// Is makes the error match ErrValueType.
func (*ValueTypeError) Is(target error) bool { return target == ErrValueType }

// ConditionError reports a condition whose operands could not be compared,
// for example a context value that is a number for a STARTSWITH action. The
// condition does not match, unless a handler set with
// WithConditionErrorHandler decides otherwise. The error names the
// condition, never its operands.
type ConditionError struct {
	Feature string
	Rule    string
	Key     string
	Action  string
	// ExceptionType is the Python exception the failure stands for:
	// TypeError, AttributeError or ValueError.
	ExceptionType string
	Err           error
}

func (e *ConditionError) Error() string {
	return fmt.Sprintf("failed to evaluate feature flag condition: feature=%s, rule=%s, key=%s, action=%s, exception_type=%s: %v",
		e.Feature, e.Rule, e.Key, e.Action, e.ExceptionType, e.Err)
}

// Unwrap returns the cause, which matches ErrOperandType,
// ErrOperandNotString or ErrOperandNotList.
func (e *ConditionError) Unwrap() error { return e.Err }

// pyError is a comparison failure, as the Python exception it stands
// for: its class name, its message, and the sentinel it matches.
type pyError struct {
	class    string
	message  string
	sentinel error
}

func (e *pyError) Error() string { return e.message }

func (e *pyError) Unwrap() error { return e.sentinel }

func typeErrorf(format string, args ...any) error {
	return &pyError{class: "TypeError", message: fmt.Sprintf(format, args...), sentinel: ErrOperandType}
}

func attributeErrorf(format string, args ...any) error {
	return &pyError{class: "AttributeError", message: fmt.Sprintf(format, args...), sentinel: ErrOperandNotString}
}

func valueErrorf(format string, args ...any) error {
	return &pyError{class: "ValueError", message: fmt.Sprintf(format, args...), sentinel: ErrOperandNotList}
}

func schemaErrorf(format string, args ...any) error {
	return &SchemaValidationError{Message: fmt.Sprintf(format, args...)}
}
