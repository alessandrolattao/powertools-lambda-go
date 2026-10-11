# Event source data classes

The `eventsource` package is the Go side of the [Powertools for AWS Lambda
(Python) event source data
classes](https://docs.powertools.aws.dev/lambda/python/latest/utilities/data_classes/).
In Go, [`aws-lambda-go/events`](https://pkg.go.dev/github.com/aws/aws-lambda-go/events)
already gives a typed struct for most event sources; `eventsource` adds what
it lacks.

## Key features

* Types for the events aws-lambda-go has no struct for.
* Extensions of the aws-lambda-go types that miss members.
* Helpers to decode nested data: base64 bodies, gzip and zlib payloads,
  DynamoDB attribute values, URL-encoded S3 keys, JSON-encoded members.
* Case-insensitive header lookups and cookies on every HTTP event.
* Response builders: API Gateway authorizer policies, Transfer Family, S3
  Batch Operations, Firehose transformations.

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go
```

### Use the event type in your handler

Declare the event type as the handler's parameter, as with any aws-lambda-go
event:

```go
func handle(ctx context.Context, event eventsource.VPCLatticeEventV2) (events.APIGatewayProxyResponse, error) {
	accept, _ := eventsource.Header(event, "accept")
	ids := eventsource.QueryParameterValues(event, "id")
	...
}

func main() {
	lambda.Start(handle)
}
```

The types work with `lambda.Start` (which decodes with `encoding/json`) and
with `encoding/json/v2` (the `parser` package). For typed payloads with
validation, see the [parser](parser.md).

### Extended types

When an aws-lambda-go type misses members, `eventsource` embeds it and adds
them. Every member of the embedded type stays reachable; the member that
had to change type is declared next to it and shadows the embedded one:

```go
func handle(ctx context.Context, event eventsource.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	org := event.RequestContext.Identity.PrincipalOrgID // added
	path := event.Path                                  // from events.APIGatewayProxyRequest
	...
}
```

Read the shadowed member through the extension (`event.RequestContext`), not
through the embedded value (`event.APIGatewayProxyRequest.RequestContext`
stays empty).

## Supported event sources

| Event source | Type | Status in aws-lambda-go |
|---|---|---|
| Active MQ | `ActiveMQEvent` | extended (`connectionId`, `persistent`) |
| API Gateway authorizer (REQUEST) | `APIGatewayCustomAuthorizerRequestTypeRequest` | extended (version, identity source, full context, WebSocket members) |
| API Gateway authorizer (TOKEN, v2) | `events.APIGatewayCustomAuthorizerRequest`, `events.APIGatewayV2CustomAuthorizerV2Request` | complete |
| API Gateway proxy | `APIGatewayProxyRequest` | extended (`version`, `principalOrgId`) |
| API Gateway proxy v2, ALB, Function URL, WebSocket | `events` types | complete |
| AppSync authorizer | `events.AppSyncLambdaAuthorizerRequest` | complete |
| AppSync resolver (direct, batch, Amplify) | `AppSyncResolverEvent` (`[]AppSyncResolverEvent` for batches) | new |
| AppSync Events | `AppSyncEventsEvent` | new |
| AWS Config rule | `ConfigEvent` | extended (`evaluationMode`, invoking event decoding) |
| Bedrock agent | `BedrockAgentEvent`, `BedrockAgentFunctionEvent` | new |
| CloudFormation custom resource | `CloudFormationCustomResourceEvent` | extended (`ServiceToken`) |
| CloudWatch alarm state change action | `CloudWatchAlarmEvent` | new |
| CloudWatch dashboard custom widget | `CloudWatchDashboardCustomWidgetEvent` | new |
| CloudWatch Logs | `events.CloudwatchLogsEvent` + `ParseCloudWatchLogs` | data extended (`policyLevel`, `extractedFields`) |
| CodeDeploy lifecycle hook | `CodeDeployLifecycleHookEvent` | new |
| CodePipeline job | `CodePipelineJobEvent` | extended (`encryptionKey`, `expirationTime`) |
| Cognito user pool triggers | `events` types; `CognitoEventUserPoolsMigrateUser`, `CognitoEventUserPoolsCustomMessage` | extended (`enableSMSMFA`, `linkParameter`) |
| Cognito custom email and SMS sender | `CognitoEventUserPoolsCustomSender` | new |
| Connect contact flow | `ConnectEvent` | extended (`MediaStreams`, nullable endpoints and queue) |
| DynamoDB Streams | `events.DynamoDBEvent`; `DynamoDBTimeWindowEvent` | window state extended |
| EventBridge | `EventBridgeEvent` | extended (`replay-name`) |
| IoT Core registry events | `IoTCoreThingEvent` and five more | new |
| Kafka | see the `kafka` package | |
| Kinesis streams | `events.KinesisEvent`; `KinesisTimeWindowEvent` | window state extended |
| Kinesis Firehose | `events.KinesisFirehoseEvent`; `KinesisFirehoseResponse` | response extended |
| Rabbit MQ, S3, S3 Batch, Secrets Manager, SNS, SQS | `events` types | complete |
| S3 Object Lambda | `S3ObjectLambdaEvent` | extended (`userName`) |
| S3 EventBridge notification | `S3EventBridgeNotificationEvent` | new |
| SES | `SimpleEmailEvent` | extended (`bcc`, `sender`, `replyTo`) |
| Transfer Family authorizer | `TransferFamilyAuthorizerEvent` | new |
| VPC Lattice | `VPCLatticeEvent`, `VPCLatticeEventV2` | new |

### HTTP events

`Body`, `JSONBody`, `Header`, `HeaderValues`, `QueryParameter`,
`QueryParameterValues`, `Cookies`, `Path` and `Method` accept every HTTP
event: API Gateway REST, HTTP and WebSocket APIs, ALB, Function URLs, VPC
Lattice v1 and v2.

```go
event := events.APIGatewayProxyRequest{
	Headers: map[string]string{"Content-Type": "application/json", "Cookie": "session=abc; theme=dark"},
	Body:    `{"id":"o-1"}`,
}

contentType, _ := eventsource.Header(event, "content-type") // application/json
session := eventsource.Cookies(event)["session"]             // abc
type order struct {
	ID string `json:"id"`
}
o, err := eventsource.JSONBody[order](event)
```

Header names match case-insensitively; query string parameters exactly.
`Body` base64-decodes when the event says so. `Path` strips the stage of an
HTTP API from the raw path. `LookupHeader` does the case-insensitive lookup
on any header map (AppSync request headers, authorizer headers).

### API Gateway authorizer

`NewAuthorizerResponse` builds the IAM policy of a REST API Lambda
authorizer; `NewWebSocketAuthorizerResponse` that of a WebSocket API.

```go
func authorize(ctx context.Context, event events.APIGatewayCustomAuthorizerRequest) (eventsource.AuthorizerPolicyResponse, error) {
	if event.AuthorizationToken != "allow" {
		return eventsource.DenyAllResponse(), nil
	}
	arn, err := eventsource.ParseRouteARN(event.MethodArn)
	if err != nil {
		return eventsource.AuthorizerPolicyResponse{}, err
	}
	response := eventsource.NewAuthorizerResponse("user-1", arn,
		eventsource.WithAuthorizerContext(map[string]any{"tenant": "t1"}))
	if err := response.AllowRoute(eventsource.HTTPVerbGet, "/orders/*"); err != nil {
		return eventsource.AuthorizerPolicyResponse{}, err
	}
	err = response.AllowRoute(eventsource.HTTPVerbPost, "/orders",
		eventsource.Condition{"IpAddress": {"aws:SourceIp": []string{"203.0.113.0/24"}}})
	if err != nil {
		return eventsource.AuthorizerPolicyResponse{}, err
	}
	return response.Build()
}
```

For HTTP APIs with simple responses, return
`events.APIGatewayV2CustomAuthorizerSimpleResponse`.

### AppSync resolver

```go
func resolve(ctx context.Context, event eventsource.AppSyncResolverEvent) (any, error) {
	switch event.Field() {
	case "getOrder":
		return getOrder(event.Arguments["id"])
	}
	if event.Identity != nil && event.Identity.Authorization() == eventsource.AppSyncAuthorizationCognito {
		// event.Identity.Sub, event.Identity.Groups...
	}
	...
}
```

`Field` and `ParentType` read the `info` object, or the top-level members of
Amplify GraphQL Transformer payloads. `MakeID`, `AWSDate`, `AWSTime`,
`AWSDateTime` and `AWSTimestamp` produce AppSync scalar values.

### AWS Config rule

```go
invoking, err := event.DecodeInvokingEvent()
switch e := invoking.(type) {
case *eventsource.ConfigConfigurationChanged:
	// e.ConfigurationItem
case *eventsource.ConfigOversizedConfiguration:
	// e.ConfigurationItemSummary
case *eventsource.ConfigScheduled:
	// e.AWSAccountID
}
params, err := event.DecodeRuleParameters[map[string]string]()
```

### CloudWatch Logs

`ParseCloudWatchLogs` decodes the subscription payload (base64, then gzip or
zlib, then JSON). For CloudWatch Logs delivered through Kinesis,
`ExtractCloudWatchLogs` and `ExtractCloudWatchLogsFromRecord` decode each
record.

```go
func handle(ctx context.Context, event events.CloudwatchLogsEvent) error {
	data, err := eventsource.ParseCloudWatchLogs(event.AWSLogs)
	if err != nil {
		return err
	}
	for _, logEvent := range data.LogEvents {
		// logEvent.Message, logEvent.ExtractedFields
	}
	return nil
}
```

### CodePipeline job

The job's helpers read the user parameters and the artifacts;
`eventsource/codepipeline` reads and writes artifacts in S3 with the job's
temporary credentials.

```go
func handle(ctx context.Context, job eventsource.CodePipelineJobEvent) error {
	params, err := job.DecodeUserParameters[map[string]string]()
	if err != nil {
		return err
	}
	artifacts := codepipeline.FromConfig(cfg, job)
	template, err := artifacts.GetFile(ctx, "SourceArtifact", "template.yaml")
	if err != nil {
		return err
	}
	return artifacts.Put(ctx, "BuildArtifact", bytes.NewReader(render(template, params)), "application/zip")
}
```

The artifact credentials never reach logs: `fmt` and `log/slog` print the
secret members as `[SENSITIVE]`.

### DynamoDB Streams

`DynamoDBImage` turns an image into Go values (`Number` for numbers, so
that all 38 digits survive); `DecodeDynamoDBImage` decodes it into a struct.

```go
type order struct {
	ID    string  `json:"pk"`
	Total float64 `json:"total"`
}
for _, record := range event.Records {
	o, err := eventsource.DecodeDynamoDBImage[order](record.Change.NewImage)
	...
}
```

### S3, S3 Batch Operations, SQS, SNS

* `UnquoteS3Key` decodes object keys as Python's `unquote_plus`, without
  failing on malformed escapes.
* `S3BatchTaskBucket` and `S3BatchTaskKey` read tasks of both schema
  versions; `NewS3BatchJobResponse` and `ValidateS3BatchJobResponse` build
  the response.
* `SQSQueueURL` turns an event source ARN into the queue URL, in every
  partition; `ParseSQSAttributes` parses the system attributes;
  `NestedS3Event` and `NestedSNSMessage` decode what S3 and SNS deliver to a
  queue.
* `SNSMessageAttributes` types the message attributes of a notification.

### Kinesis Firehose

```go
func transform(ctx context.Context, event events.KinesisFirehoseEvent) (eventsource.KinesisFirehoseResponse, error) {
	var response eventsource.KinesisFirehoseResponse
	for _, record := range event.Records {
		response.Add(eventsource.KinesisFirehoseResponseRecord{
			RecordID: record.RecordID,
			Result:   events.KinesisFirehoseTransformedStateOk,
			Data:     bytes.ToUpper(record.Data),
		})
	}
	return response, response.Validate()
}
```

Unlike `events.KinesisFirehoseResponse`, a record without metadata leaves it
out.

### Transfer Family

```go
func authenticate(ctx context.Context, event eventsource.TransferFamilyAuthorizerEvent) (eventsource.TransferFamilyAuthorizerResponse, error) {
	if !checkPassword(event.Username, event.Password) {
		return eventsource.TransferFamilyAuthorizerResponse{}, nil // empty: access denied
	}
	return eventsource.NewTransferFamilyS3Response("arn:aws:iam::123456789012:role/sftp-user",
		eventsource.WithHomeDirectory("/my-bucket/home/"+event.Username))
}
```

## Testing your code

The types are plain structs: build them in tests, or decode a sample event.

```go
func TestHandle(t *testing.T) {
	raw, err := os.ReadFile("testdata/vpcLatticeV2Event.json")
	require.NoError(t, err)
	var event eventsource.VPCLatticeEventV2
	require.NoError(t, json.Unmarshal(raw, &event))

	response, err := handle(t.Context(), event)
	require.NoError(t, err)
	assert.Equal(t, 200, response.StatusCode)
}
```

## Differences from Powertools Python

* Plain structs instead of dictionary wrappers: members are struct fields,
  decoded once; helpers compute on each call and return errors instead of
  raising `KeyError`.
* JSON helpers are generic and decode into your type.
* DynamoDB numbers keep every digit (`Number`), binary values are bytes and
  sets are slices.
* AppSync identities tell Cognito, OIDC, IAM and Lambda apart; AppSync Events
  with no events get no placeholder event.
* Not ported: the `event_source` decorator (handlers are typed already), the
  deprecated `get_header_value`, the advancing `record`/`message` iterators;
  the ALB query decoding switch and the response header serializers belong
  to the event handler.
* Upstream bugs fixed: authorizer conditions are written as an IAM
  `Condition` object and the ARN partition is kept; queue URLs use the
  partition's DNS suffix; the HTTP API stage is stripped only when the path
  starts with it; the EFS Transfer Family response carries the uid; VPC
  Lattice v2 multi-value headers stay lists; IoT timestamps are UTC; the S3
  EventBridge `version-id` is optional; CloudWatch Logs accept zlib; tumbling
  window state can hold any JSON value.
