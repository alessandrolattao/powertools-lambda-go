package validation

import (
	"errors"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// fuzzSchema is a fixed schema with defaults, references and combinators,
// for events of any shape.
var fuzzSchema = MustCompile(`{
	"definitions": {"item": {"properties": {"qty": {"default": 1}}, "additionalProperties": {"default": 0}}},
	"anyOf": [
		{"type": "object", "properties": {"items": {"items": {"$ref": "#/definitions/item"}}, "s": {"default": "x"}}},
		{"type": "array", "items": [{"default": 1}, {"$ref": "#/definitions/item"}]},
		{"type": ["string", "number", "boolean", "null"]}
	]
}`)

// FuzzValidate feeds arbitrary schemas, events and envelopes to the
// validator: whatever they hold, it returns data or an error, never panics.
func FuzzValidate(f *testing.F) {
	f.Add(orderSchema, `{"id":"o-1","items":[{"sku":"a"}]}`, "")
	f.Add(`{"items":[{"default":1},{"default":2}]}`, `[]`, "")
	f.Add(`{"anyOf":[{"properties":{"a":{"default":1}}},{"type":"array"}]}`, `{}`, "")
	f.Add(`{"$ref":"#/definitions/a","definitions":{"a":{"$ref":"#/definitions/a"}}}`, `1`, "")
	f.Add(`{"type":"array","items":{"required":["id"]}}`, `{"Records":[{"body":"{\"id\":1}"}]}`, SQS)
	f.Add(`{"format":"email"}`, `"a@b"`, "")
	f.Add(`{"properties":{"a b/c~d":{"maximum":4}}}`, `{"a b/c~d":60}`, "")
	f.Fuzz(func(t *testing.T, schema, event, envelope string) {
		opts := []Option{WithFormatRegexp("digits", `\d+`)}
		if envelope != "" {
			opts = append(opts, WithEnvelope(envelope))
		}
		if _, err := fuzzSchema.Validate([]byte(event), opts...); err != nil &&
			!errors.Is(err, ErrInvalidEvent) && !errors.Is(err, ErrSchemaValidation) && !errors.Is(err, jmespath.ErrInvalidEnvelope) {
			t.Fatalf("unexpected error: %v", err)
		}
		compiled, err := Compile(schema, opts...)
		if err != nil {
			return
		}
		data, err := compiled.Validate([]byte(event), opts...)
		if err != nil {
			if verr, ok := err.(*SchemaValidationError); ok && verr.Message == "" { //nolint:errorlint // the concrete type is what is checked
				t.Fatal("empty message")
			}
			return
		}
		// The validated data, defaults included, is still valid JSON.
		if _, err := ValidateAs[any](data, `{}`); err != nil {
			t.Fatalf("validated data does not round-trip: %v", err)
		}
	})
}
