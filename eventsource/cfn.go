package eventsource

import (
	"github.com/aws/aws-lambda-go/cfn"
)

// CloudFormationCustomResourceEvent is cfn.Event (a CloudFormation custom
// resource request) with the service token, the ARN the template invokes.
// Respond with cfn.NewResponse and Response.Send, or wrap the handler with
// cfn.LambdaWrap.
type CloudFormationCustomResourceEvent struct {
	cfn.Event

	ServiceToken string `json:"ServiceToken"`
}
