package datamasking

import (
	"errors"
	"fmt"
)

// ErrDataMasking is what every error the package detects matches with
// errors.Is, as Python's DataMaskingError is the base of its exceptions.
var ErrDataMasking = errors.New("datamasking")

// The failures the package and its providers report. Each one matches
// ErrDataMasking too.
var (
	// ErrUnsupportedType reports data that cannot be masked or encrypted:
	// a value encoding/json/v2 cannot marshal, JSON that does not parse, or
	// a scalar given to field selection, which needs an object or an array.
	ErrUnsupportedType = fmt.Errorf("%w: unsupported data type", ErrDataMasking)
	// ErrEmptyFields reports WithFields called with no expression.
	ErrEmptyFields = fmt.Errorf("%w: fields parameter cannot be empty", ErrDataMasking)
	// ErrFieldNotFound reports an expression that matches nothing, when the
	// DataMasking raises on missing fields (the default).
	ErrFieldNotFound = fmt.Errorf("%w: field or expression not found", ErrDataMasking)
	// ErrInvalidPath reports an expression outside the accepted JSONPath
	// dialect (see the package documentation).
	ErrInvalidPath = fmt.Errorf("%w: invalid masking path", ErrDataMasking)
	// ErrMaskField reports an Eraser that failed on a selected value with
	// an error of its own.
	ErrMaskField = fmt.Errorf("%w: failed to mask field at path", ErrDataMasking)
	// ErrInvalidRegex reports a regular expression, or a mask format, that
	// does not compile.
	ErrInvalidRegex = fmt.Errorf("%w: invalid regex pattern", ErrDataMasking)
	// ErrNoProvider reports Encrypt or Decrypt on a DataMasking built
	// without a provider: erasing needs none, encrypting does.
	ErrNoProvider = fmt.Errorf("%w: no encryption provider configured", ErrDataMasking)
	// ErrEncryptKey reports a provider that could not obtain or wrap a data
	// key, typically a KMS key that is not a symmetric key ARN the function
	// may use.
	ErrEncryptKey = fmt.Errorf("%w: failed to encrypt data, use a valid symmetric AWS KMS key ARN, not a key ID or alias", ErrDataMasking)
	// ErrDecryptKey reports a provider that could not decrypt any data key of
	// the message with the keys it was given.
	ErrDecryptKey = fmt.Errorf("%w: failed to decrypt data, use a valid symmetric AWS KMS key ARN, not a key ID or alias", ErrDataMasking)
	// ErrDecryptValue reports ciphertext that is not a message the provider
	// produced: bad base64, a malformed or tampered message, or a plaintext
	// that is not JSON.
	ErrDecryptValue = fmt.Errorf("%w: data decryption failed, decrypt only data that was previously encrypted", ErrDataMasking)
	// ErrContextMismatch reports a decryption whose encryption context is
	// not exactly the one the data was encrypted with.
	ErrContextMismatch = fmt.Errorf("%w: encryption context does not match, use the exact context used during encryption", ErrDataMasking)
)

// PathError reports a failure tied to one field expression or masking
// rule.
type PathError struct {
	// Path is the expression, as the caller wrote it.
	Path string
	// Err is ErrFieldNotFound, ErrInvalidPath or ErrMaskField.
	Err error
	// Cause is what went wrong underneath, when there is more to say.
	Cause error
}

func (e *PathError) Error() string {
	msg := fmt.Sprintf("%v %q", e.Err, e.Path)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the sentinel and the cause, so that errors.Is matches
// both.
func (e *PathError) Unwrap() []error {
	if e.Cause == nil {
		return []error{e.Err}
	}
	return []error{e.Err, e.Cause}
}
