package parser

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// Errors of the envelopes.
var (
	// ErrInvalidEnvelope reports an event that does not have the shape of
	// the envelope's event source.
	ErrInvalidEnvelope = errors.New("event does not match the envelope")
	// ErrInvalidPayload reports a payload that cannot be decoded: bad
	// base64, compressed data that does not inflate, text that is not UTF-8
	// or not JSON.
	ErrInvalidPayload = errors.New("invalid payload")
)

// Envelope unwraps the payload of an event source from a raw event, for
// Parse and EventParser. The package has one for each source Powertools
// Python has an envelope for; implement it to unwrap your own events.
type Envelope interface {
	// Unwrap returns the payload of the event as JSON (null when the event
	// has none) or, for sources that batch records, a JSON array of the
	// payloads.
	Unwrap(event jsontext.Value) (jsontext.Value, error)
}

// payload is one payload found in an event.
type payload struct {
	// data is the JSON text of the payload, nil when the event has none.
	data []byte
	// source says where the payload is in the event.
	source string
}

// envelope is the Envelope of events of type E, whose payloads extract
// finds. A batch envelope unwraps to a JSON array.
type envelope[E any] struct {
	batch   bool
	extract func(event *E) ([]payload, error)
}

// Unwrap decodes the event and returns its payloads as JSON.
func (e envelope[E]) Unwrap(raw jsontext.Value) (jsontext.Value, error) {
	var event E
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("%w %T: %w", ErrInvalidEnvelope, event, err)
	}
	payloads, err := e.extract(&event)
	if err != nil {
		return nil, err
	}
	if !e.batch {
		if len(payloads) == 0 {
			return jsontext.Value("null"), nil
		}
		return payloadJSON(payloads[0])
	}
	return payloadArray(payloads)
}

// payloadJSON returns the payload's JSON text, null when it is missing.
func payloadJSON(p payload) (jsontext.Value, error) {
	if len(p.data) == 0 {
		return jsontext.Value("null"), nil
	}
	value := jsontext.Value(p.data)
	if !value.IsValid() {
		return nil, fmt.Errorf("%w: %s is not JSON", ErrInvalidPayload, p.source)
	}
	return value, nil
}

func payloadArray(payloads []payload) (jsontext.Value, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, p := range payloads {
		if i > 0 {
			buf.WriteByte(',')
		}
		value, err := payloadJSON(p)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// one decodes the single payload of event into a T.
func one[T, E any](env envelope[E], event *E, opts []Option) (T, error) {
	var zero T
	payloads, err := env.extract(event)
	if err != nil {
		return zero, parseError[T]("", err)
	}
	if len(payloads) == 0 {
		return zero, nil
	}
	return decode[T](payloads[0].data, payloads[0].source, newConfig(opts).json)
}

// many decodes each payload of event into a T.
func many[T, E any](env envelope[E], event *E, opts []Option) ([]T, error) {
	payloads, err := env.extract(event)
	if err != nil {
		return nil, parseError[T]("", err)
	}
	return decodeAll[T](payloads, newConfig(opts).json)
}

func decodeAll[T any](payloads []payload, opts []json.Options) ([]T, error) {
	out := make([]T, 0, len(payloads))
	for _, p := range payloads {
		v, err := decode[T](p.data, p.source, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// body is the payload of an HTTP request body, base64-decoded when the event
// says it is encoded. Powertools Python ignores isBase64Encoded and fails on
// encoded bodies.
func body(text string, isBase64Encoded bool) ([]payload, error) {
	const source = "body"
	if text == "" {
		return []payload{{source: source}}, nil
	}
	if !isBase64Encoded {
		return []payload{{data: []byte(text), source: source}}, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not base64: %w", ErrInvalidPayload, source, err)
	}
	return []payload{{data: decoded, source: source}}, nil
}

// text is a payload held in a string.
func text(value, source string) payload {
	if value == "" {
		return payload{source: source}
	}
	return payload{data: []byte(value), source: source}
}

// APIGatewayRESTEnvelope unwraps the body of an API Gateway REST API proxy
// request (payload version 1.0), Python's ApiGatewayEnvelope.
var APIGatewayRESTEnvelope Envelope = apiGatewayREST

var apiGatewayREST = envelope[events.APIGatewayProxyRequest]{
	extract: func(e *events.APIGatewayProxyRequest) ([]payload, error) {
		return body(e.Body, e.IsBase64Encoded)
	},
}

// APIGatewayREST decodes the body of an API Gateway REST API proxy request
// into a T. A request without body gives the zero T.
func APIGatewayREST[T any](event events.APIGatewayProxyRequest, opts ...Option) (T, error) {
	return one[T](apiGatewayREST, &event, opts)
}

// APIGatewayHTTPEnvelope unwraps the body of an API Gateway HTTP API request
// (payload version 2.0), Python's ApiGatewayV2Envelope.
var APIGatewayHTTPEnvelope Envelope = apiGatewayHTTP

var apiGatewayHTTP = envelope[events.APIGatewayV2HTTPRequest]{
	extract: func(e *events.APIGatewayV2HTTPRequest) ([]payload, error) {
		return body(e.Body, e.IsBase64Encoded)
	},
}

// APIGatewayHTTP decodes the body of an API Gateway HTTP API request into a
// T.
func APIGatewayHTTP[T any](event events.APIGatewayV2HTTPRequest, opts ...Option) (T, error) {
	return one[T](apiGatewayHTTP, &event, opts)
}

// APIGatewayWebSocketEnvelope unwraps the body of an API Gateway WebSocket
// API message, Python's ApiGatewayWebSocketEnvelope.
var APIGatewayWebSocketEnvelope Envelope = apiGatewayWebSocket

var apiGatewayWebSocket = envelope[events.APIGatewayWebsocketProxyRequest]{
	extract: func(e *events.APIGatewayWebsocketProxyRequest) ([]payload, error) {
		return body(e.Body, e.IsBase64Encoded)
	},
}

// APIGatewayWebSocket decodes the body of an API Gateway WebSocket API
// message into a T.
func APIGatewayWebSocket[T any](event events.APIGatewayWebsocketProxyRequest, opts ...Option) (T, error) {
	return one[T](apiGatewayWebSocket, &event, opts)
}

// FunctionURLEnvelope unwraps the body of a Lambda Function URL request,
// Python's LambdaFunctionUrlEnvelope.
var FunctionURLEnvelope Envelope = functionURL

var functionURL = envelope[events.LambdaFunctionURLRequest]{
	extract: func(e *events.LambdaFunctionURLRequest) ([]payload, error) {
		return body(e.Body, e.IsBase64Encoded)
	},
}

// FunctionURL decodes the body of a Lambda Function URL request into a T.
func FunctionURL[T any](event events.LambdaFunctionURLRequest, opts ...Option) (T, error) {
	return one[T](functionURL, &event, opts)
}

// VPCLatticeEnvelope unwraps the body of a VPC Lattice request (payload
// version 1), Python's VpcLatticeEnvelope.
var VPCLatticeEnvelope Envelope = vpcLattice

var vpcLattice = envelope[eventsource.VPCLatticeEvent]{
	extract: func(e *eventsource.VPCLatticeEvent) ([]payload, error) {
		return body(e.Body, e.IsBase64Encoded)
	},
}

// VPCLattice decodes the body of a VPC Lattice request (payload version 1)
// into a T.
func VPCLattice[T any](event eventsource.VPCLatticeEvent, opts ...Option) (T, error) {
	return one[T](vpcLattice, &event, opts)
}

// VPCLatticeV2Envelope unwraps the body of a VPC Lattice request (payload
// version 2), Python's VpcLatticeV2Envelope.
var VPCLatticeV2Envelope Envelope = vpcLatticeV2

var vpcLatticeV2 = envelope[eventsource.VPCLatticeEventV2]{
	extract: func(e *eventsource.VPCLatticeEventV2) ([]payload, error) {
		return body(e.Body, e.IsBase64Encoded)
	},
}

// VPCLatticeV2 decodes the body of a VPC Lattice request (payload version 2)
// into a T.
func VPCLatticeV2[T any](event eventsource.VPCLatticeEventV2, opts ...Option) (T, error) {
	return one[T](vpcLatticeV2, &event, opts)
}

// EventBridgeEnvelope unwraps the detail of an EventBridge event, Python's
// EventBridgeEnvelope.
var EventBridgeEnvelope Envelope = eventBridge

var eventBridge = envelope[events.EventBridgeEvent]{
	extract: func(e *events.EventBridgeEvent) ([]payload, error) {
		detail := []byte(e.Detail)
		// EventBridge Scheduler sends an empty detail as the string "{}".
		if e.Source == "aws.scheduler" && bytes.Equal(bytes.TrimSpace(detail), []byte(`"{}"`)) {
			detail = []byte("{}")
		}
		return []payload{{data: detail, source: "detail"}}, nil
	},
}

// EventBridge decodes the detail of an EventBridge event into a T.
func EventBridge[T any](event events.EventBridgeEvent, opts ...Option) (T, error) {
	return one[T](eventBridge, &event, opts)
}

// SQSEnvelope unwraps the body of every message of an SQS event, Python's
// SqsEnvelope.
var SQSEnvelope Envelope = sqs

var sqs = envelope[events.SQSEvent]{
	batch: true,
	extract: func(e *events.SQSEvent) ([]payload, error) {
		out := make([]payload, 0, len(e.Records))
		for i := range e.Records {
			out = append(out, text(e.Records[i].Body, "Records["+strconv.Itoa(i)+"].body"))
		}
		return out, nil
	},
}

// SQS decodes the body of every message of an SQS event into a T.
func SQS[T any](event events.SQSEvent, opts ...Option) ([]T, error) {
	return many[T](sqs, &event, opts)
}

// SNSEnvelope unwraps the message of every record of an SNS event, Python's
// SnsEnvelope.
var SNSEnvelope Envelope = sns

var sns = envelope[events.SNSEvent]{
	batch: true,
	extract: func(e *events.SNSEvent) ([]payload, error) {
		out := make([]payload, 0, len(e.Records))
		for i := range e.Records {
			out = append(out, text(e.Records[i].SNS.Message, "Records["+strconv.Itoa(i)+"].Sns.Message"))
		}
		return out, nil
	},
}

// SNS decodes the message of every record of an SNS event into a T.
func SNS[T any](event events.SNSEvent, opts ...Option) ([]T, error) {
	return many[T](sns, &event, opts)
}

// SNSSQSEnvelope unwraps the SNS message inside the body of every message of
// an SQS queue subscribed to an SNS topic (without raw message delivery),
// Python's SnsSqsEnvelope.
var SNSSQSEnvelope Envelope = snsSQS

var snsSQS = envelope[events.SQSEvent]{
	batch: true,
	extract: func(e *events.SQSEvent) ([]payload, error) {
		out := make([]payload, 0, len(e.Records))
		for i := range e.Records {
			source := "Records[" + strconv.Itoa(i) + "].body"
			notification, err := eventsource.NestedSNSMessage(e.Records[i])
			if err != nil {
				return nil, fmt.Errorf("%w: %s: %w", ErrInvalidPayload, source, err)
			}
			out = append(out, text(notification.Message, source+".Message"))
		}
		return out, nil
	},
}

// SNSSQS decodes the SNS message inside the body of every message of an SQS
// event into a T.
func SNSSQS[T any](event events.SQSEvent, opts ...Option) ([]T, error) {
	return many[T](snsSQS, &event, opts)
}

// CloudWatchLogsEnvelope unwraps the message of every log event of a
// CloudWatch Logs subscription, Python's CloudWatchLogsEnvelope.
var CloudWatchLogsEnvelope Envelope = cloudWatchLogs

var cloudWatchLogs = envelope[events.CloudwatchLogsEvent]{
	batch: true,
	extract: func(e *events.CloudwatchLogsEvent) ([]payload, error) {
		data, err := eventsource.ParseCloudWatchLogs(e.AWSLogs)
		if err != nil {
			return nil, fmt.Errorf("%w: awslogs.data: %w", ErrInvalidPayload, err)
		}
		out := make([]payload, 0, len(data.LogEvents))
		for i := range data.LogEvents {
			out = append(out, text(data.LogEvents[i].Message, "awslogs.data.logEvents["+strconv.Itoa(i)+"].message"))
		}
		return out, nil
	},
}

// CloudWatchLogs decodes the message of every log event of a CloudWatch
// Logs subscription into a T.
func CloudWatchLogs[T any](event events.CloudwatchLogsEvent, opts ...Option) ([]T, error) {
	return many[T](cloudWatchLogs, &event, opts)
}

// KinesisEnvelope unwraps the data of every record of a Kinesis Data Streams
// event, Python's KinesisDataStreamEnvelope. Data that is not UTF-8 text is
// inflated as gzip or zlib first.
var KinesisEnvelope Envelope = kinesis

var kinesis = envelope[events.KinesisEvent]{
	batch: true,
	extract: func(e *events.KinesisEvent) ([]payload, error) {
		out := make([]payload, 0, len(e.Records))
		for i := range e.Records {
			source := "Records[" + strconv.Itoa(i) + "].kinesis.data"
			data := e.Records[i].Kinesis.Data
			if !utf8.Valid(data) {
				inflated, err := eventsource.Decompress(data)
				if err != nil || !utf8.Valid(inflated) {
					return nil, fmt.Errorf("%w: %s: unable to decode and/or decompress data", ErrInvalidPayload, source)
				}
				data = inflated
			}
			out = append(out, payload{data: data, source: source})
		}
		return out, nil
	},
}

// Kinesis decodes the data of every record of a Kinesis Data Streams event
// into a T.
func Kinesis[T any](event events.KinesisEvent, opts ...Option) ([]T, error) {
	return many[T](kinesis, &event, opts)
}

// FirehoseEnvelope unwraps the data of every record of an Amazon Data
// Firehose transformation event, Python's KinesisFirehoseEnvelope.
var FirehoseEnvelope Envelope = firehose

var firehose = envelope[events.KinesisFirehoseEvent]{
	batch: true,
	extract: func(e *events.KinesisFirehoseEvent) ([]payload, error) {
		out := make([]payload, 0, len(e.Records))
		for i := range e.Records {
			source := "records[" + strconv.Itoa(i) + "].data"
			if !utf8.Valid(e.Records[i].Data) {
				return nil, fmt.Errorf("%w: %s is not UTF-8", ErrInvalidPayload, source)
			}
			out = append(out, payload{data: e.Records[i].Data, source: source})
		}
		return out, nil
	},
}

// Firehose decodes the data of every record of a Firehose transformation
// event into a T.
func Firehose[T any](event events.KinesisFirehoseEvent, opts ...Option) ([]T, error) {
	return many[T](firehose, &event, opts)
}

// BedrockAgentEnvelope unwraps the input text of a Bedrock agent OpenAPI
// action group event, Python's BedrockAgentEnvelope.
var BedrockAgentEnvelope Envelope = bedrockAgent

var bedrockAgent = envelope[eventsource.BedrockAgentEvent]{
	extract: func(e *eventsource.BedrockAgentEvent) ([]payload, error) {
		return []payload{text(e.InputText, "inputText")}, nil
	},
}

// BedrockAgent decodes the input text of a Bedrock agent event into a T.
func BedrockAgent[T any](event eventsource.BedrockAgentEvent, opts ...Option) (T, error) {
	return one[T](bedrockAgent, &event, opts)
}

// BedrockAgentFunctionEnvelope unwraps the input text of a Bedrock agent
// function action group event, Python's BedrockAgentFunctionEnvelope.
var BedrockAgentFunctionEnvelope Envelope = bedrockAgentFunction

var bedrockAgentFunction = envelope[eventsource.BedrockAgentFunctionEvent]{
	extract: func(e *eventsource.BedrockAgentFunctionEvent) ([]payload, error) {
		return []payload{text(e.InputText, "inputText")}, nil
	},
}

// BedrockAgentFunction decodes the input text of a Bedrock agent function
// event into a T.
func BedrockAgentFunction[T any](event eventsource.BedrockAgentFunctionEvent, opts ...Option) (T, error) {
	return one[T](bedrockAgentFunction, &event, opts)
}
