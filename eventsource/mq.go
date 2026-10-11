package eventsource

import (
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

// ActiveMQEvent is events.ActiveMQEvent whose messages carry the members
// aws-lambda-go leaves out.
type ActiveMQEvent struct {
	events.ActiveMQEvent

	Messages []ActiveMQMessage `json:"messages"`
}

// ActiveMQMessage is events.ActiveMQMessage with the JMS connection id and
// the persistence flag.
type ActiveMQMessage struct {
	events.ActiveMQMessage

	ConnectionID string `json:"connectionId,omitempty"`
	Persistent   bool   `json:"persistent,omitempty"`
}

// DecodedData returns the message body, base64-decoded.
func (m ActiveMQMessage) DecodedData() ([]byte, error) {
	return decodeMQData(m.Data)
}

// DecodeMQData decodes the base64 body of an Amazon MQ message, ActiveMQ or
// RabbitMQ (events.RabbitMQMessage.Data).
func DecodeMQData(data string) ([]byte, error) {
	return decodeMQData(data)
}

func decodeMQData(data string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decode the base64 message data: %w", err)
	}
	return decoded, nil
}
