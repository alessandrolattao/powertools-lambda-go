// Package eventsource is the Go side of Powertools for AWS Lambda (Python)'s
// event source data classes: the event types aws-lambda-go lacks, the
// members its partial types leave out, and the helpers Powertools adds on top
// of them.
//
// github.com/aws/aws-lambda-go/events already has a struct for most event
// sources, and a handler receives it typed. This package does not redeclare
// any of them. It adds:
//
//   - the events aws-lambda-go has no type for: AppSync resolvers and AppSync
//     Events, Bedrock agents, CloudWatch alarm actions and dashboard custom
//     widgets, CodeDeploy lifecycle hooks, Cognito custom senders, the IoT
//     Core registry events, S3 notifications through EventBridge, Transfer
//     Family authorizers and VPC Lattice (payload versions 1 and 2);
//   - extensions of the aws-lambda-go types that miss members Powertools
//     reads (ActiveMQ, API Gateway REST and REQUEST authorizers, CloudWatch
//     Logs, CloudFormation, CodePipeline, Cognito, Config, Connect,
//     EventBridge, Firehose responses, S3 Object Lambda, SES, Kinesis and
//     DynamoDB tumbling windows);
//   - helpers: request bodies and headers of every HTTP source, cookies, the
//     API Gateway authorizer policy builder, DynamoDB attribute values to Go
//     values, S3 key decoding, SQS queue URLs and nested S3 and SNS events,
//     gzip and zlib payloads, CodePipeline artifacts (subpackage
//     eventsource/codepipeline), Transfer Family and S3 Batch Operations
//     responses, AppSync scalars.
//
// # Extended types
//
// An extension embeds the aws-lambda-go type and declares the missing
// members next to it, shadowing the embedded member that must change type:
//
//	type APIGatewayProxyRequest struct {
//		events.APIGatewayProxyRequest
//
//		Version        string                        `json:"version,omitempty"`
//		RequestContext APIGatewayProxyRequestContext `json:"requestContext"`
//	}
//
// encoding/json (v1, used by lambda.Start) and encoding/json/v2 both decode
// the shadowing member and ignore the embedded one, so read the context as
// e.RequestContext; e.APIGatewayProxyRequest.RequestContext stays empty.
// Every other member is promoted, and Go 1.27 composite literals can set
// promoted members directly. When the container of an extended element would
// have to change too (SES records, the CodePipeline job), the container is
// redeclared, still embedding what it can.
//
// Kafka events belong to the kafka package.
//
// # HTTP events
//
// Body, JSONBody, Header, HeaderValues, QueryParameter, QueryParameterValues,
// Cookies, Path and Method accept any HTTP event (the HTTPRequest type set:
// API Gateway REST, HTTP and WebSocket APIs, ALB, Function URLs, VPC Lattice):
//
//	func handle(ctx context.Context, event events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
//		token, _ := eventsource.Header(event, "authorization")
//		order, err := eventsource.JSONBody[Order](event)
//		...
//	}
//
// Header names match case-insensitively, query string parameters exactly.
// Body decodes base64 bodies. Cookies parses as leniently as Powertools
// Python. Path strips the HTTP API stage from the raw path.
//
// # Decoding is lenient
//
// As in aws-lambda-go, unknown members are ignored and missing ones stay
// zero; nothing is validated. Members Powertools treats as optional are
// pointers or carry omitempty. A few members accept more than one JSON shape
// because AWS sends more than one: VPC Lattice v2 headers (StringValues),
// its timeEpoch (EpochMicros), the AppSync prev object. For typed payloads
// with validation, see the parser package.
//
// # Differences from Python
//
//   - No dictionary wrapper: the types are plain structs, decoded once; the
//     Python properties that only read a member are the struct fields.
//   - Helpers return errors instead of raising KeyError or IndexError, and
//     compute on each call instead of caching.
//   - JSON helpers (json_body, decoded_user_parameters, reason_data_decoded,
//     rule_parameters) are generic and decode into a type of your choice.
//   - DynamoDB numbers are Number values that keep every digit, binary
//     values are bytes (Python keeps the base64 text), sets are slices.
//   - The deprecated get_header_value, the advancing iterators of Kafka and
//     ActiveMQ events and the event_source decorator are not ported: Go
//     handlers are typed already and slices are iterated with range.
//   - AppSync identities are one struct whose Authorization method tells
//     Cognito, OIDC, IAM and Lambda identities apart (Python only tells
//     Cognito from IAM). AppSync Events with no events are not given a
//     placeholder event.
//   - The ALB query parameter decoding switch and the response header
//     serializers belong to the event handler.
//
// Upstream bugs fixed: the authorizer policy writes Condition as the object
// IAM expects (Python writes a list) and keeps the ARN partition; SQSQueueURL
// uses the partition's DNS suffix; the HTTP API path is only stripped of a
// stage it starts with; the EFS Transfer Family response carries the uid
// (Python writes the gid twice); VPC Lattice v2 headers and parameters with
// several values are kept as lists; IoT timestamps are UTC; the version-id of
// an S3 EventBridge object is optional; CloudWatch Logs payloads may be zlib
// as well as gzip; Kinesis and DynamoDB tumbling window state may hold any
// JSON value.
package eventsource
