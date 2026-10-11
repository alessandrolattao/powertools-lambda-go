package validation

import "github.com/alessandrolattao/powertools-lambda-go/jmespath"

// The built-in envelopes of Powertools Python's validation.envelopes: the
// JMESPath expressions that select the payload of the common event sources,
// for WithEnvelope. SQS, Kinesis and CloudWatch Logs select a list, so their
// schema describes an array of payloads; SNS selects the message of the
// first record only, as in Python.
const (
	APIGatewayREST            = jmespath.APIGatewayREST
	APIGatewayHTTP            = jmespath.APIGatewayHTTP
	SQS                       = jmespath.SQS
	SNS                       = jmespath.SNS
	EventBridge               = jmespath.EventBridge
	CloudWatchEventsScheduled = jmespath.CloudWatchEventsSchedule
	KinesisDataStream         = jmespath.KinesisDataStream
	CloudWatchLogs            = jmespath.CloudWatchLogs
)
