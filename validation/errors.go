package validation

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors, which the error types of this package match with
// errors.Is.
var (
	// ErrSchemaValidation is matched by every *SchemaValidationError.
	ErrSchemaValidation = errors.New("failed schema validation")
	// ErrInvalidSchema is matched by every *InvalidSchemaFormatError.
	ErrInvalidSchema = errors.New("invalid schema")
	// ErrInvalidEvent reports data that is not JSON, or does not marshal to
	// it.
	ErrInvalidEvent = errors.New("event is not valid JSON")
	// ErrNoLoader reports a "$ref" to a remote document whose URL scheme
	// has no loader (see WithLoader and WithHTTPLoader).
	ErrNoLoader = errors.New("no loader for the URL scheme")
	// ErrFetch reports a remote document the HTTP loader could not fetch.
	ErrFetch = errors.New("fetch remote schema")
)

// SchemaValidationError reports data that breaks the schema, with the
// fields of Powertools Python's exception of the same name. The engine
// collects every failure; this error describes the first one, as Python's
// fail-fast engine does, and unwraps to the engine's
// *jsonschema.ValidationError, whose BasicOutput lists them all.
type SchemaValidationError struct {
	// Message is the full message, in Python's format: "Failed schema
	// validation. Error: <ValidationMessage>, Path: <Path>, Data: <Value>".
	Message string
	// ValidationMessage says what is wrong, in fastjsonschema's words:
	// "data.property[1] must be smaller than or equal to 42".
	ValidationMessage string
	// Name is the location of the value in the data: "data.property[1]".
	Name string
	// Path is the location as a list: ["data", "property", "1"].
	Path []string
	// Value is the value that breaks the schema.
	Value any
	// Definition is the schema the value breaks: {"type": "number",
	// "maximum": 42}.
	Definition any
	// Rule is the keyword the value breaks: "maximum".
	Rule string
	// RuleDefinition is the value of that keyword: 42.
	RuleDefinition any
	// Err is the engine's error.
	Err error
}

func (e *SchemaValidationError) Error() string { return e.Message }

// Unwrap returns the engine's error.
func (e *SchemaValidationError) Unwrap() error { return e.Err }

// Is makes every SchemaValidationError match ErrSchemaValidation.
func (*SchemaValidationError) Is(target error) bool { return target == ErrSchemaValidation }

// InvalidSchemaFormatError reports a schema that cannot be compiled: it is
// not valid JSON, breaks its metaschema, references a document no loader can
// provide, or declares a format pattern that does not compile.
type InvalidSchemaFormatError struct {
	// Schema is the JSON text of the schema, when it could be produced.
	Schema string
	// Formats lists the names of the custom formats.
	Formats []string
	// Err is the cause.
	Err error
}

func (e *InvalidSchemaFormatError) Error() string {
	return fmt.Sprintf("Schema received: %s, Formats: [%s]. Error: %v", e.Schema, strings.Join(e.Formats, ", "), e.Err)
}

// Unwrap returns the cause.
func (e *InvalidSchemaFormatError) Unwrap() error { return e.Err }

// Is makes every InvalidSchemaFormatError match ErrInvalidSchema.
func (*InvalidSchemaFormatError) Is(target error) bool { return target == ErrInvalidSchema }
