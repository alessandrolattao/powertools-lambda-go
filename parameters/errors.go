package parameters

import (
	"errors"
	"fmt"
)

// The sentinel errors every failure of this package and of the providers
// below it matches with errors.Is.
var (
	// ErrGetParameter: a value could not be read from its store.
	ErrGetParameter = errors.New("failed to get parameter")
	// ErrTransform: a value was read but its transform failed, or the Go type
	// asked for cannot hold it.
	ErrTransform = errors.New("failed to transform parameter")
	// ErrSetParameter: an SSM parameter could not be written.
	ErrSetParameter = errors.New("failed to set parameter")
	// ErrGetSecret: several secrets could not be read in one batch.
	ErrGetSecret = errors.New("failed to get secrets")
	// ErrSetSecret: a secret could not be written.
	ErrSetSecret = errors.New("failed to set secret")
	// ErrNotFound: the store holds nothing under the name asked for. Stores
	// whose SDK has its own not-found error (SSM, Secrets Manager) wrap that
	// one instead; DynamoDB and custom stores use this.
	ErrNotFound = errors.New("parameter not found")
)

// GetParameterError reports a value that could not be read: Python's
// GetParameterError.
type GetParameterError struct {
	// Name is the name, or the path, that was read.
	Name string
	Err  error
}

func (e *GetParameterError) Error() string {
	return fmt.Sprintf("get parameter %q: %v", e.Name, e.Err)
}

// Unwrap returns the cause.
func (e *GetParameterError) Unwrap() error { return e.Err }

// Is makes the error match ErrGetParameter.
func (*GetParameterError) Is(target error) bool { return target == ErrGetParameter }

// TransformError reports a value whose transform failed: Python's
// TransformParameterError.
type TransformError struct {
	// Name is the name of the value.
	Name string
	// Transform is the transform asked for.
	Transform Transform
	Err       error
}

func (e *TransformError) Error() string {
	return fmt.Sprintf("parameter %q: unable to transform value using %q transform: %v", e.Name, string(e.Transform), e.Err)
}

// Unwrap returns the cause.
func (e *TransformError) Unwrap() error { return e.Err }

// Is makes the error match ErrTransform.
func (*TransformError) Is(target error) bool { return target == ErrTransform }

// SetParameterError reports an SSM parameter that could not be written:
// Python's SetParameterError.
type SetParameterError struct {
	Name string
	Err  error
}

func (e *SetParameterError) Error() string {
	return fmt.Sprintf("error setting parameter %q: %v", e.Name, e.Err)
}

// Unwrap returns the cause.
func (e *SetParameterError) Unwrap() error { return e.Err }

// Is makes the error match ErrSetParameter.
func (*SetParameterError) Is(target error) bool { return target == ErrSetParameter }

// GetSecretError reports a batch of secrets that could not be read:
// Python's GetSecretError. A single secret that cannot be read is a
// GetParameterError, as in Python.
type GetSecretError struct {
	// Names are the secrets asked for.
	Names []string
	Err   error
}

func (e *GetSecretError) Error() string {
	return fmt.Sprintf("get secrets %q: %v", e.Names, e.Err)
}

// Unwrap returns the cause.
func (e *GetSecretError) Unwrap() error { return e.Err }

// Is makes the error match ErrGetSecret.
func (*GetSecretError) Is(target error) bool { return target == ErrGetSecret }

// SetSecretError reports a secret that could not be written: Python's
// SetSecretError.
type SetSecretError struct {
	Name string
	Err  error
}

func (e *SetSecretError) Error() string {
	return fmt.Sprintf("error setting secret %q: %v", e.Name, e.Err)
}

// Unwrap returns the cause.
func (e *SetSecretError) Unwrap() error { return e.Err }

// Is makes the error match ErrSetSecret.
func (*SetSecretError) Is(target error) bool { return target == ErrSetSecret }
