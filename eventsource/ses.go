package eventsource

import (
	"github.com/aws/aws-lambda-go/events"
)

// SimpleEmailEvent is events.SimpleEmailEvent (an Amazon SES receipt rule
// invocation) whose common headers have the Bcc, Sender and Reply-To
// addresses.
type SimpleEmailEvent struct {
	Records []SimpleEmailRecord `json:"Records"`
}

// SimpleEmailRecord is events.SimpleEmailRecord with the complete message.
type SimpleEmailRecord struct {
	events.SimpleEmailRecord

	SES SimpleEmailService `json:"ses"`
}

// SimpleEmailService is events.SimpleEmailService with the complete message.
type SimpleEmailService struct {
	events.SimpleEmailService

	Mail SimpleEmailMessage `json:"mail"`
}

// SimpleEmailMessage is events.SimpleEmailMessage with the complete common
// headers.
type SimpleEmailMessage struct {
	events.SimpleEmailMessage

	CommonHeaders SimpleEmailCommonHeaders `json:"commonHeaders"`
}

// SimpleEmailCommonHeaders is events.SimpleEmailCommonHeaders with the
// headers aws-lambda-go leaves out.
type SimpleEmailCommonHeaders struct {
	events.SimpleEmailCommonHeaders

	Bcc     []string `json:"bcc,omitempty"`
	Sender  []string `json:"sender,omitempty"`
	ReplyTo []string `json:"replyTo,omitempty"`
}
