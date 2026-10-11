package eventsource

import (
	"github.com/aws/aws-lambda-go/events"
)

// CognitoEventUserPoolsCustomSender is the event of the Cognito custom email
// sender and custom SMS sender triggers: the message Cognito wants sent,
// with the code encrypted with the pool's KMS key (decrypt it with the AWS
// Encryption SDK before sending it).
type CognitoEventUserPoolsCustomSender struct {
	events.CognitoEventUserPoolsHeader

	Request CognitoEventUserPoolsCustomSenderRequest `json:"request"`
}

// CognitoEventUserPoolsCustomSenderRequest is the request of a custom sender
// trigger.
type CognitoEventUserPoolsCustomSenderRequest struct {
	// Type is the request version, customEmailSenderRequestV1 or
	// customSMSSenderRequestV1.
	Type string `json:"type"`
	// Code is the encrypted code, empty for some trigger sources.
	Code           string            `json:"code"`
	UserAttributes map[string]any    `json:"userAttributes"`
	ClientMetadata map[string]string `json:"clientMetadata,omitempty"`
}

// CognitoEventUserPoolsCustomEmailSender is the event of the custom email
// sender trigger.
type CognitoEventUserPoolsCustomEmailSender = CognitoEventUserPoolsCustomSender

// CognitoEventUserPoolsCustomSMSSender is the event of the custom SMS sender
// trigger.
type CognitoEventUserPoolsCustomSMSSender = CognitoEventUserPoolsCustomSender

// CognitoEventUserPoolsMigrateUser is events.CognitoEventUserPoolsMigrateUser
// whose response can turn SMS multi-factor authentication on.
type CognitoEventUserPoolsMigrateUser struct {
	events.CognitoEventUserPoolsMigrateUser

	Response CognitoEventUserPoolsMigrateUserResponse `json:"response"`
}

// CognitoEventUserPoolsMigrateUserResponse is
// events.CognitoEventUserPoolsMigrateUserResponse with enableSMSMFA.
type CognitoEventUserPoolsMigrateUserResponse struct {
	events.CognitoEventUserPoolsMigrateUserResponse

	EnableSMSMFA bool `json:"enableSMSMFA,omitempty"`
}

// CognitoEventUserPoolsCustomMessage is
// events.CognitoEventUserPoolsCustomMessage whose request carries the link
// parameter.
type CognitoEventUserPoolsCustomMessage struct {
	events.CognitoEventUserPoolsCustomMessage

	Request CognitoEventUserPoolsCustomMessageRequest `json:"request"`
}

// CognitoEventUserPoolsCustomMessageRequest is
// events.CognitoEventUserPoolsCustomMessageRequest with the placeholder for
// the verification link.
type CognitoEventUserPoolsCustomMessageRequest struct {
	events.CognitoEventUserPoolsCustomMessageRequest

	LinkParameter string `json:"linkParameter,omitempty"`
}
