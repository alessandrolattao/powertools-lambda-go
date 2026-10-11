package eventsource

import (
	"encoding/json/v2"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

// SNSMessageAttribute is a message attribute of an SNS notification.
type SNSMessageAttribute struct {
	// Type is String, String.Array, Number or Binary.
	Type string `json:"Type"`
	// Value is the attribute value; base64 for Binary.
	Value string `json:"Value"`
}

// SNSMessageAttributes returns the typed message attributes of a
// notification; events.SNSEntity keeps them as untyped maps.
func SNSMessageAttributes(entity events.SNSEntity) (map[string]SNSMessageAttribute, error) {
	out := make(map[string]SNSMessageAttribute, len(entity.MessageAttributes))
	if len(entity.MessageAttributes) == 0 {
		return out, nil
	}
	raw, err := json.Marshal(entity.MessageAttributes)
	if err != nil {
		return nil, fmt.Errorf("encode the message attributes: %w", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode the message attributes: %w", err)
	}
	return out, nil
}
