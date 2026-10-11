# JMESPath functions

Package `jmespath` extracts data from events with
[JMESPath](https://jmespath.site) expressions, and adds the functions
Powertools uses to decode what AWS event sources encode. Validation,
idempotency, the logger's correlation id and feature flags accept the same
expressions.

## Key features

- Deserialize JSON strings, base64 and gzip payloads inside an expression:
  `powertools_json`, `powertools_base64`, `powertools_base64_gzip`.
- Ready-made envelopes for the common event sources.
- Bring your own functions, next to the built-in and the Powertools ones.
- Compiled expressions, safe for concurrent invocations.

## Getting started

### Extracting data

`Query` evaluates an expression against an event: an aws-lambda-go struct, a
map, or raw JSON. `QueryAs` decodes the selection into a Go type.

```go
event := events.APIGatewayV2HTTPRequest{Body: `{"customerId":"c-42"}`}

payload, err := jmespath.Query(event, jmespath.APIGatewayHTTP+".customerId")
// payload == "c-42"

orders, err := jmespath.QueryAs[[]order](sqsEvent, jmespath.SQS)
```

Compile an expression once, at package level or in `main`, and search on
every invocation:

```go
var userID = jmespath.MustCompile("requestContext.authorizer.jwt.claims.sub")

sub, err := userID.Search(event)
```

### Built-in envelopes

| Constant | Expression |
|---|---|
| `APIGatewayREST`, `APIGatewayHTTP` | `powertools_json(body)` |
| `SQS` | `Records[*].powertools_json(body)` |
| `SNS` | `Records[0].Sns.Message \| powertools_json(@)` |
| `EventBridge`, `CloudWatchEventsSchedule` | `detail` |
| `KinesisDataStream` | `Records[*].kinesis.powertools_json(powertools_base64(data))` |
| `CloudWatchLogs` | `awslogs.powertools_base64_gzip(data) \| powertools_json(@).logEvents[*]` |
| `S3SNSSQS` | `Records[*].powertools_json(body).powertools_json(Message).Records[0]` |
| `S3SQS` | `Records[*].powertools_json(body).Records[0]` |
| `S3SNSKinesisFirehose` | `records[*].powertools_json(powertools_base64(data)).powertools_json(Message).Records[0]` |
| `S3KinesisFirehose` | `records[*].powertools_json(powertools_base64(data)).Records[0]` |
| `S3EventBridgeSQS` | `Records[*].powertools_json(body).detail` |

## Advanced

### Built-in functions

- `powertools_json(string)` parses a JSON string.
- `powertools_base64(string)` decodes base64 into UTF-8 text.
- `powertools_base64_gzip(string)` decodes base64, then gunzips it, as
  CloudWatch Logs subscriptions deliver their data. Inflation stops at
  `MaxDecompressedSize` (128 MiB), so that a small malicious payload cannot
  exhaust the function's memory.

### Bring your own function

```go
upper := jmespath.Function{
	Name: "upper",
	Args: []jmespath.Arg{{Types: []jmespath.Type{jmespath.TypeString}}},
	Call: func(args []any) (any, error) {
		s, _ := args[0].(string)
		return strings.ToUpper(s), nil
	},
}
name, err := jmespath.Query(event, "upper(powertools_json(body).name)", jmespath.WithFunctions(upper))
```

### Errors

Every failure to compile or evaluate an expression is an `*EnvelopeError`
carrying the expression, and matches `ErrInvalidEnvelope` with `errors.Is`.

## Differences from Powertools Python

- Expressions follow JMESPath Community Edition, a superset of the
  specification Powertools Python implements.
- Numbers are float64: integers above 2^53 lose precision.
- Raw JSON with duplicate member names keeps the last one, as Python's `json`
  module does.

## Testing your code

`Query` is a pure function: feed it the event your test builds, or a sample
event read from a file, and assert on the result.
