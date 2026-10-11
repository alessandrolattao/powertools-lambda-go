package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// JSON is a field that AWS sends as a string holding JSON, such as the body
// of an SQS message or the message of an SNS notification: Pydantic's
// Json[T]. It decodes the string, then its content into Value:
//
//	type OrderMessage struct {
//		events.SQSMessage
//
//		Body parser.JSON[Order] `json:"body"`
//	}
//
// A field that already holds the JSON value itself, not a string, decodes
// too. It marshals back as a string holding JSON, the shape AWS sends.
type JSON[T any] struct {
	Value T
}

// UnmarshalJSON decodes a JSON string holding JSON, or a JSON value, into
// Value.
func (j *JSON[T]) UnmarshalJSON(data []byte) error {
	content := data
	if jsontext.Value(data).Kind() == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("decode the JSON string: %w", err)
		}
		content = []byte(text)
	}
	var value T
	if err := json.Unmarshal(content, &value); err != nil {
		return fmt.Errorf("decode the JSON content into %T: %w", value, err)
	}
	j.Value = value
	return nil
}

// MarshalJSON encodes Value as JSON inside a JSON string.
func (j JSON[T]) MarshalJSON() ([]byte, error) {
	content, err := json.Marshal(j.Value)
	if err != nil {
		return nil, fmt.Errorf("encode the JSON content: %w", err)
	}
	text, err := json.Marshal(string(content))
	if err != nil {
		return nil, fmt.Errorf("encode the JSON string: %w", err)
	}
	return text, nil
}

// Validate validates Value when it is a Validator, so that a model holding
// JSON fields can validate them from its own Validate method.
func (j JSON[T]) Validate() error {
	return validate(&j.Value)
}
