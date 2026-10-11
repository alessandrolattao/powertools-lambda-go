// Package parser parses Lambda events, or the payload an event source wraps,
// into typed Go models and validates them: the Go side of Powertools for AWS
// Lambda (Python)'s parser utility, without Pydantic.
//
// The models are Go structs: the aws-lambda-go events, the eventsource
// types, and your own. Parse decodes data into one with encoding/json/v2,
// then calls its Validate method, if it has one:
//
//	type Order struct {
//		ID    string  `json:"id"`
//		Total float64 `json:"total"`
//	}
//
//	func (o Order) Validate() error {
//		if o.ID == "" {
//			return errors.New("id is required")
//		}
//		return nil
//	}
//
//	order, err := parser.Parse[Order](event.Body)
//
// Validate is the counterpart of Pydantic's field and model validators. It
// is called on the decoded value and, when the model is a slice or an
// array, on each element. A failure, of decoding or of Validate, is a
// *ParseError, which carries the model, where the payload was and the cause.
//
// # Envelopes
//
// An envelope unwraps the payload of an event source: the body of an API
// Gateway request, the detail of an EventBridge event, the body of every
// SQS message. Each of Python's 17 envelopes is here twice:
//
//   - a typed helper, for a handler that already receives the aws-lambda-go
//     event: SQS, SNS, SNSSQS, EventBridge, APIGatewayREST, APIGatewayHTTP,
//     APIGatewayWebSocket, FunctionURL, CloudWatchLogs, Kinesis, Firehose,
//     Kafka, DynamoDBStream, VPCLattice, VPCLatticeV2, BedrockAgent,
//     BedrockAgentFunction:
//
//     func handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
//     orders, err := parser.SQS[Order](event)
//     ...
//     }
//
//   - an Envelope value (SQSEnvelope, EventBridgeEnvelope...), for Parse and
//     EventParser on the raw event. A source that batches records unwraps to
//     a JSON array, so the model is a slice:
//
//     orders, err := parser.Parse[[]Order](raw, parser.WithEnvelope(parser.SQSEnvelope))
//
// Payloads that are strings holding JSON (bodies, messages) are parsed as
// JSON; Kinesis and Firehose data and Kafka values are base64-decoded first,
// and Kinesis data that is not text is inflated as gzip or zlib. A missing
// payload (a GET request has no body) gives the zero model, not validated,
// as Python gives None: use a pointer model to tell it apart. DynamoDB
// stream records give DynamoDBImages, with nil for an image the record does
// not carry.
//
// Implement Envelope to unwrap your own events, as Python's BaseEnvelope.
//
// # The decorator
//
// EventParser is Python's @event_parser: it turns a handler of a model into
// a handler of raw events that parses each event before calling it.
//
//	lambda.Start(parser.EventParser(handle, parser.WithEnvelope(parser.EventBridgeEnvelope)))
//
// # Nested JSON and extended models
//
// JSON[T] is a field AWS sends as a string holding JSON, Pydantic's Json[T].
// Extending a built-in model, as Python subclasses one to type its payload,
// is Go embedding: the field declared next to the embedded event replaces
// the embedded one with the same JSON name.
//
//	type OrderEvent struct {
//		events.EventBridgeEvent
//
//		Detail Order `json:"detail"`
//	}
//
//	type OrderMessage struct {
//		events.SQSMessage
//
//		Body parser.JSON[Order] `json:"body"`
//	}
//
// # Decoding rules
//
// Decoding is lenient, as aws-lambda-go's: unknown members are ignored,
// missing ones stay zero; Validate checks what is required.
// encoding/json/v2 matches member names case-sensitively, except for the SNS
// notifications inside SQS messages, which spell two members differently
// (UnsubscribeURL for UnsubscribeUrl) and decode either way.
// WithJSONOptions passes options to the decoder: json.RejectUnknownMembers
// for Pydantic's extra="forbid", for example.
//
// # Differences from Python
//
//   - No Pydantic: there are no built-in model validators. The Literal and
//     format checks of Python's built-in models (eventSource values, HTTP
//     methods, source IPs) are not enforced; the envelopes decode the outer
//     event leniently, like aws-lambda-go, and Validate checks the payload.
//   - The model is a type parameter, inferred by EventParser from the
//     handler: there is no model argument and no InvalidModelTypeError.
//   - A missing payload is the zero value of the model, not None.
//   - The HTTP envelopes honor isBase64Encoded; Python fails on encoded
//     bodies.
//   - Kafka: the typed helper reads events.KafkaEvent, whose records are a
//     map; it orders them by topic partition. KafkaEnvelope keeps the
//     event's order, as Python does.
//   - DynamoDB numbers keep every digit as JSON numbers; binary attributes
//     are base64 strings, sets are arrays.
//   - Pydantic's lax coercion ("123" into an int) does not happen; use the
//     json "string" tag option where AWS sends numbers as strings.
package parser
