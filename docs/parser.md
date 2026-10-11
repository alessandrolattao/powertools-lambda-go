# Parser

The parser decodes Lambda events, or the payload an event source wraps,
into typed Go models and validates them. It is the Go port of the
[Powertools for AWS Lambda (Python) parser
utility](https://docs.powertools.aws.dev/lambda/python/latest/utilities/parser/),
without Pydantic: models are Go structs, decoding is `encoding/json/v2`,
validation is a `Validate() error` method.

## Key features

* Decode an event or its payload into a Go struct, with validation.
* Built-in envelopes to unwrap the payload of 17 event sources, as typed
  helpers and as envelopes for raw events.
* `EventParser`, a decorator that parses the event before your handler runs.
* `JSON[T]` for fields AWS sends as strings holding JSON.
* Errors that say which record failed and why.

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go
```

### Data model with parser

A model is a Go struct with `json` tags. Give it a `Validate` method to check
what decoding cannot: required fields, ranges, formats.

```go
type Order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

func (o Order) Validate() error {
	if o.ID == "" {
		return errors.New("id is required")
	}
	return nil
}
```

#### Event parser

`EventParser` is Python's `@event_parser`. It turns a handler of your model
into a handler of raw events: every invocation is parsed and validated
before the handler runs, and a failure is returned as a `*parser.ParseError`.
The model type is the handler's event type.

```go
func handle(ctx context.Context, orders []Order) (string, error) {
	return fmt.Sprintf("%d orders", len(orders)), nil
}

func main() {
	lambda.Start(parser.EventParser(handle, parser.WithEnvelope(parser.SQSEnvelope)))
}
```

#### Parse function

`Parse` decodes data into a model and validates it. Data is raw JSON
(`[]byte`, `jsontext.Value`, or a `string` holding JSON text) or any value
that marshals to JSON.

```go
order, err := parser.Parse[Order](`{"id":"o-1","total":12.5}`)
if err != nil {
	var parseErr *parser.ParseError
	if errors.As(err, &parseErr) {
		// parseErr.Model, parseErr.Source and parseErr.Err say what failed.
	}
}
```

#### Key differences between Parse and EventParser

| | `EventParser` | `Parse` |
|---|---|---|
| Where | wraps the handler | anywhere in your code |
| Failure | the invocation fails before the handler runs | you handle the error |
| Input | the raw event | raw JSON or any value |

### Built-in models

The built-in models are the event types of
[`aws-lambda-go/events`](https://pkg.go.dev/github.com/aws/aws-lambda-go/events)
and of the `eventsource` package (for the events aws-lambda-go lacks: VPC
Lattice, Bedrock agents, AppSync, IoT registry, ...). Parse into them like
into your own models:

```go
event, err := parser.Parse[events.EventBridgeEvent](raw)
```

#### Extending built-in models

Python subclasses a built-in model to type its payload. In Go, embed the
event and declare the payload field next to it: the field you declare
replaces the embedded one with the same JSON name.

```go
type OrderEvent struct {
	events.EventBridgeEvent

	Detail Order `json:"detail"`
}

event, err := parser.Parse[OrderEvent](raw)
// event.Detail.ID, event.Source, event.Time...
```

## Advanced

### Envelopes

An envelope unwraps the payload of an event source before parsing. When you
only care about the payload, parse it straight into your model.

Each envelope comes twice:

* a typed helper, for handlers that already receive the aws-lambda-go event:

    ```go
    func handle(ctx context.Context, event events.SQSEvent) error {
    	orders, err := parser.SQS[Order](event)
    	...
    }
    ```

* an `Envelope` value, for `Parse` and `EventParser` on the raw event:

    ```go
    orders, err := parser.Parse[[]Order](raw, parser.WithEnvelope(parser.SQSEnvelope))
    ```

Sources that batch records unwrap to a list, so the model is a slice. A
missing payload (an API Gateway GET has no body) gives the zero value of the
model, not validated, as Python gives `None`: use a pointer model, such as
`*Order`, to tell it apart.

#### Built-in envelopes

| Python envelope | Typed helper | Envelope | Payload | Result |
|---|---|---|---|---|
| `ApiGatewayEnvelope` | `APIGatewayREST` | `APIGatewayRESTEnvelope` | `body` (base64-decoded when flagged) | `T` |
| `ApiGatewayV2Envelope` | `APIGatewayHTTP` | `APIGatewayHTTPEnvelope` | `body` | `T` |
| `ApiGatewayWebSocketEnvelope` | `APIGatewayWebSocket` | `APIGatewayWebSocketEnvelope` | `body` | `T` |
| `LambdaFunctionUrlEnvelope` | `FunctionURL` | `FunctionURLEnvelope` | `body` | `T` |
| `VpcLatticeEnvelope` | `VPCLattice` | `VPCLatticeEnvelope` | `body` | `T` |
| `VpcLatticeV2Envelope` | `VPCLatticeV2` | `VPCLatticeV2Envelope` | `body` | `T` |
| `EventBridgeEnvelope` | `EventBridge` | `EventBridgeEnvelope` | `detail` | `T` |
| `SqsEnvelope` | `SQS` | `SQSEnvelope` | `Records[*].body` | `[]T` |
| `SnsEnvelope` | `SNS` | `SNSEnvelope` | `Records[*].Sns.Message` | `[]T` |
| `SnsSqsEnvelope` | `SNSSQS` | `SNSSQSEnvelope` | SNS `Message` in each SQS body | `[]T` |
| `CloudWatchLogsEnvelope` | `CloudWatchLogs` | `CloudWatchLogsEnvelope` | `logEvents[*].message` | `[]T` |
| `KinesisDataStreamEnvelope` | `Kinesis` | `KinesisEnvelope` | `Records[*].kinesis.data` (gzip or zlib inflated when not text) | `[]T` |
| `KinesisFirehoseEnvelope` | `Firehose` | `FirehoseEnvelope` | `records[*].data` | `[]T` |
| `KafkaEnvelope` | `Kafka` | `KafkaEnvelope` | every record `value` | `[]T` |
| `DynamoDBStreamEnvelope` | `DynamoDBStream` | `DynamoDBStreamEnvelope` | `NewImage` and `OldImage` | `[]DynamoDBImages[T]` |
| `BedrockAgentEnvelope` | `BedrockAgent` | `BedrockAgentEnvelope` | `inputText` | `T` |
| `BedrockAgentFunctionEnvelope` | `BedrockAgentFunction` | `BedrockAgentFunctionEnvelope` | `inputText` | `T` |

DynamoDB stream images arrive as attribute values; the envelope turns them
into plain JSON first (numbers keep every digit, binary values become base64
strings), so your model uses ordinary `json` tags:

```go
records, err := parser.DynamoDBStream[Order](event)
for _, record := range records {
	if record.NewImage != nil {
		// an INSERT or a MODIFY
	}
}
```

The Kafka typed helper reads `events.KafkaEvent`, whose records are a map and
lose their order; it sorts them by topic partition. `KafkaEnvelope` reads the
raw event and keeps its order.

#### Bringing your own envelope

Implement `parser.Envelope`: return the payload as JSON (or a JSON array of
payloads).

```go
type ordersEnvelope struct{}

func (ordersEnvelope) Unwrap(event jsontext.Value) (jsontext.Value, error) {
	var wrapper struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(event, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Data, nil
}

order, err := parser.Parse[Order](raw, parser.WithEnvelope(ordersEnvelope{}))
```

### Data model validation

`Validate` is called on the decoded model, and on every element when the
model is a slice or an array (each record of a batch envelope). It plays the
part of Pydantic's field and model validators: check single fields or the
whole model, and return an error to reject the event.

```go
func (o Order) Validate() error {
	switch {
	case o.ID == "":
		return errors.New("id is required")
	case o.Total < 0:
		return fmt.Errorf("total %v is negative", o.Total)
	}
	return nil
}
```

A failure is a `*ParseError`: `Model` is the Go type, `Source` where the
payload was (`Records[2].body`), `Err` the cause (a `*json.SemanticError`, a
`*jsontext.SyntacticError` or your error). Every `ParseError` matches
`parser.ErrParse`.

Decoding is lenient, as aws-lambda-go's: unknown members are ignored and
missing ones stay zero. Pass decoder options with `WithJSONOptions`, for
example `json.RejectUnknownMembers(true)`, Pydantic's `extra="forbid"`.

#### String fields that contain JSON data

`parser.JSON[T]` is Pydantic's `Json[T]`: a field AWS sends as a string
holding JSON. It decodes the string, then its content into `Value`.

```go
type OrderMessage struct {
	events.SQSMessage

	Body parser.JSON[Order] `json:"body"`
}

message, err := parser.Parse[OrderMessage](raw)
// message.Body.Value.ID
```

### Serialization

Models are Go structs: marshal them with `encoding/json/v2` and the usual tag
options (`omitzero`, `omitempty`, `string`). `JSON[T]` marshals back to a
string holding JSON, the shape AWS sends.

## Testing your code

Parsing is a pure function of the event: test it with sample events, and
test your handler through `EventParser` with raw JSON.

```go
func TestHandle(t *testing.T) {
	handler := parser.EventParser(handle, parser.WithEnvelope(parser.SQSEnvelope))
	out, err := handler(t.Context(), jsontext.Value(`{"Records":[{"body":"{\"id\":\"o-1\"}"}]}`))
	require.NoError(t, err)
	assert.Equal(t, "1 orders", out)

	_, err = handler(t.Context(), jsontext.Value(`{"Records":[{"body":"{}"}]}`))
	require.ErrorIs(t, err, parser.ErrParse)
}
```

## Differences from Powertools Python

* No Pydantic: the built-in models do not check their `Literal` fields
  (`eventSource`, HTTP methods) or formats (source IPs, URLs); the envelopes
  decode the outer event leniently and your `Validate` checks the payload.
* The model is a type parameter; `EventParser` infers it from the handler.
* A missing payload is the zero value of the model instead of `None`.
* The HTTP envelopes base64-decode bodies flagged `isBase64Encoded`; Python
  fails on them.
* The Kafka typed helper orders records by topic partition (the event is a
  map); `KafkaEnvelope` keeps the event's order, as Python does.
* DynamoDB binary attributes become base64 strings and sets arrays.
* No lax coercion: `"123"` does not decode into an `int`; use the `json`
  `string` tag option where AWS sends numbers as strings.
