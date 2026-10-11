# Validation

This utility provides JSON Schema validation for events and responses,
including JMESPath support to unwrap events before validation. It is the Go
port of the [Powertools for AWS Lambda (Python) validation
utility](https://docs.powertools.aws.dev/lambda/python/latest/utilities/validation/).

## Key features

* Validate incoming events and responses against JSON Schemas (drafts 4, 6,
  7, 2019-09 and 2020-12).
* JMESPath support to unwrap events before validation applies.
* Built-in envelopes to unwrap the payloads of popular event sources.
* Schemas compiled once and reused across invocations.
* Schema defaults filled into the validated data.
* Errors with the fields of Powertools Python's `SchemaValidationError`.

## Getting started

You can validate inbound and outbound events with the `Validator`
decorator, or call `Validate` yourself when you want more control, such as
turning a validation error into a `400` response.

!!! tip "Using JSON Schemas for the first time?"
    Check this [step-by-step tour in the official JSON Schema
    website](https://json-schema.org/learn/getting-started-step-by-step.html).

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go
```

The JSON Schema engine is
[`github.com/santhosh-tekuri/jsonschema/v6`](https://github.com/santhosh-tekuri/jsonschema).

### Schemas

A schema is raw JSON (`[]byte`, `jsontext.Value`, or a `string`, for example
one read with `go:embed`), a value that marshals to JSON (`map[string]any`),
or a `*validation.Schema` compiled with `Compile`. Compile your schemas once,
at package level, so that the work is done while the function initializes:

```go
var userSchema = validation.MustCompile(`{
	"type": "object",
	"required": ["user_id", "project"],
	"properties": {
		"user_id": {"type": "string", "format": "uuid"},
		"project": {"type": "string", "enum": ["powertools", "other"]},
		"ip": {"type": "string", "format": "ipv4"},
		"role": {"type": "string", "default": "reader"}
	}
}`)
```

`Validate` also accepts schema documents: it compiles each one on first use
and caches it.

### Validator decorator

The `Validator` decorator validates the event before your handler runs and
its response after. It fails fast with a `*validation.SchemaValidationError`
when either does not conform to its schema.

The returned handler takes the raw event (`jsontext.Value`); your handler
receives the payload (unwrapped by the envelope, with the schema defaults)
decoded into its own event type, as Powertools Python hands the unwrapped
event to the decorated function:

```go
type order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

type receipt struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`
}

func handle(_ context.Context, o order) (receipt, error) {
	return receipt{OrderID: o.ID, Status: "accepted"}, nil
}

func main() {
	lambda.Start(validation.Validator(handle,
		validation.Inbound(`{"type":"object","required":["id","total"],"properties":{"total":{"minimum":0}}}`,
			validation.WithEnvelope(validation.APIGatewayREST)),
		validation.Outbound(`{"type":"object","required":["orderId","status"]}`),
	))
}
```

It is not a requirement to validate both inbound and outbound events: use
either option, or both. `Inbound(nil, validation.WithEnvelope(...))` only
unwraps the event. The defaults of the outbound schema are filled into the
response, as Python fills them into a dict response.

### Validate function

`Validate` is typically used within the handler, or in any function that
validates data. It returns the validated data: the part of the event the
envelope selects, with the schema defaults filled in. Handle a
`*validation.SchemaValidationError` to answer gracefully:

```go
func handler(_ context.Context, event jsontext.Value) (events.APIGatewayProxyResponse, error) {
	if _, err := validation.Validate(event, userSchema); err != nil {
		if verr, ok := errors.AsType[*validation.SchemaValidationError](err); ok {
			return events.APIGatewayProxyResponse{StatusCode: 400, Body: verr.Message}, nil
		}
		return events.APIGatewayProxyResponse{}, err
	}
	return events.APIGatewayProxyResponse{StatusCode: 200}, nil
}
```

The message has Powertools Python's format:

```text
Failed schema validation. Error: data.project must be one of ['powertools', 'other'], Path: ['data', 'project'], Data: lambda
```

`ValidateAs` validates and decodes the result, defaults included, into a Go
type:

```go
type user struct {
	UserID  string `json:"user_id"`
	Project string `json:"project"`
	Role    string `json:"role"`
}

u, err := validation.ValidateAs[user](event, userSchema)
// u.Role == "reader" when the event has no role
```

### Unwrapping events prior to validation

You might want to validate only a portion of your event: this is what
`WithEnvelope` is for. Envelopes are [JMESPath
expressions](https://jmespath.org/tutorial.html) that extract the part of
the JSON to validate. Here, only the `detail` of an EventBridge event is
validated:

```go
event := events.EventBridgeEvent{
	DetailType: "UserCreated",
	Source:     "my.app",
	Detail:     jsontext.Value(`{"user_id":"0d44b083-8206-4a3a-aa95-5d392a99be4a","project":"powertools"}`),
}

detail, err := validation.Validate(event, userSchema, validation.WithEnvelope("detail"))
```

JMESPath can extract records from arrays, and combine pipes and function
expressions, so you can select exactly what you need before validating it.

### Built-in envelopes

The built-in envelopes extract the payload of popular event sources:

```go
detail, err := validation.Validate(event, userSchema, validation.WithEnvelope(validation.EventBridge))
```

| Envelope | JMESPath expression |
| --- | --- |
| `APIGatewayHTTP` | `powertools_json(body)` |
| `APIGatewayREST` | `powertools_json(body)` |
| `CloudWatchEventsScheduled` | `detail` |
| `CloudWatchLogs` | `awslogs.powertools_base64_gzip(data) \| powertools_json(@).logEvents[*]` |
| `EventBridge` | `detail` |
| `KinesisDataStream` | `Records[*].kinesis.powertools_json(powertools_base64(data))` |
| `SNS` | `Records[0].Sns.Message \| powertools_json(@)` |
| `SQS` | `Records[*].powertools_json(body)` |

`SQS`, `KinesisDataStream` and `CloudWatchLogs` select a list: their schema
describes an array, and a decorated handler receives a slice. `SNS`
selects the message of the first record only.

## Advanced

### Validating custom formats

JSON Schema Draft 7 [has many built-in
formats](https://json-schema.org/understanding-json-schema/reference/string.html#format)
such as `date`, `time`, `uuid` and `regex`, which might replace a custom
format if you control the schema. Formats are asserted for every draft, as
in Python, and an unknown format is ignored.

For a custom format such as `awsaccountid`, give a function that reports
whether a string conforms, or a regular expression:

```go
accountID := regexp.MustCompile(`^\d{12}$`)
schema := validation.MustCompile(`{"properties":{"accountid":{"type":"string","format":"awsaccountid"}}}`,
	validation.WithFormat("awsaccountid", accountID.MatchString))

_, err := schema.Validate([]byte(`{"accountid":"1234"}`))
// Failed schema validation. Error: data.accountid must be awsaccountid, Path: ['data', 'accountid'], Data: 1234
```

`WithFormatRegexp` does the same with a regular expression, such as
`\d{12}$`. As with Python's `re.match`, the pattern is anchored at the start
of the value only. Formats apply to strings; other values always conform.

### Built-in JMESPath functions

Events and responses often carry encoded JSON that needs decoding before
validation. Envelopes can call the Powertools JMESPath functions of the
`jmespath` package to deserialize JSON strings (`powertools_json`),
decode base64 (`powertools_base64`) and decompress gzip data
(`powertools_base64_gzip`). The built-in envelopes use them to unwrap
sources such as Kinesis and CloudWatch Logs. Custom functions go next to
them:

```go
validation.WithEnvelope("upper(name)", jmespath.WithFunctions(upper))
```

### Validating with external references

JSON Schema [allows schemas to reference other
schemas](https://json-schema.org/understanding-json-schema/structuring#dollarref)
with `$ref`. Powertools Python fetches remote references over HTTP by
default; the Go port fetches nothing unless asked, and a reference without a
document fails the compilation with `validation.ErrNoLoader`. You decide
where documents come from:

* `WithResource(url, schema)` preloads a document under its URL, for schemas
  split into files that ship with the function.
* `WithLoader(scheme, load)` resolves the URLs of a scheme with your own
  function (Python's `handlers`): from S3, Parameter Store, an embedded
  file system.
* `WithHTTPLoader(client)` fetches `http` and `https` URLs, as Python does.

```go
const child = `{"type":"object","properties":{"project":{"type":"string"}},"required":["project"]}`
parent := `{"type":"object","properties":{"ParentSchema":{"$ref":"https://example.com/schemas/child.json"}}}`

schema, err := validation.Compile(parent, validation.WithLoader("https", func(ctx context.Context, url string) (any, error) {
	return child, nil
}))
```

Loaders run while the schema compiles, once. Give a schema whose references
are relative URLs an `$id`: it is the base they resolve against.

### Drafts

The `$schema` keyword selects the draft. A schema without it is read as
Draft 7 (`validation.DefaultDraft`), which is what Powertools Python
applies; `WithDefaultDraft` changes that. Unlike Python, whose engine
applies the draft-07 rules to every schema, a 2019-09 or 2020-12 schema is
validated by the rules of its draft.

### Defaults

When a property missing from the data has a `default` in its schema, the
validated data gets it, as with fastjsonschema's `use_default`. Defaults are
filled into objects and tuples, following `$ref`, `allOf`, and the branch of
`anyOf`, `oneOf` and `if` that the data matches. `WithDefaults(false)` turns
this off. The caller's data is never changed: `Validate` returns a new
value.

### Other options

| Option | Python | Effect |
| --- | --- | --- |
| `WithDefaults(bool)` | `provider_options={"use_default": ...}` | fill in defaults (on) |
| `WithFormatAssertion(bool)` | `provider_options={"use_formats": ...}` | check formats (on) |
| `WithDefaultDraft(Draft)` | | draft of schemas without `$schema` |
| `WithRegexpEngine(func)` | | another regexp engine for `pattern` |

Patterns are Go regular expressions (RE2), which have no lookarounds or
backreferences. `WithRegexpEngine` plugs in an engine such as
`github.com/dlclark/regexp2` for patterns written for Python's `re`.

### Errors

| Error | `errors.Is` | When |
| --- | --- | --- |
| `*SchemaValidationError` | `ErrSchemaValidation` | the data breaks the schema |
| `*InvalidSchemaFormatError` | `ErrInvalidSchema` | the schema does not compile |
| `*jmespath.EnvelopeError` | `jmespath.ErrInvalidEnvelope` | the envelope fails |
| | `ErrInvalidEvent` | the event is not JSON |

`SchemaValidationError` has Python's fields: `Message`, `ValidationMessage`
(`data.price must be smaller than or equal to 42`), `Name`
(`data.items[0].price`), `Path` (`[data items 0 price]`), `Value`,
`Definition` (the subschema), `Rule` (`maximum`) and `RuleDefinition`
(`42`). It describes the first failure; it unwraps to the engine's
`*jsonschema.ValidationError`, whose `BasicOutput()` lists every failure.

## Testing your code

Validation needs no AWS resources: call `Validate`, or the decorated
handler, with sample events.

```go
func TestHandlerRejectsNegativeTotals(t *testing.T) {
	h := validation.Validator(handle,
		validation.Inbound(`{"type":"object","required":["id","total"],"properties":{"total":{"minimum":0}}}`,
			validation.WithEnvelope(validation.APIGatewayREST)),
	)

	_, err := h(t.Context(), jsontext.Value(`{"body":"{\"id\":\"o-2\",\"total\":-1}"}`))

	verr, ok := errors.AsType[*validation.SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "minimum", verr.Rule)
}
```

Sample events for every event source are in the [Powertools Python
repository](https://github.com/aws-powertools/powertools-lambda-python/tree/develop/tests/events).

## Differences from Powertools Python

* Schemas compile once and are cached; Python compiles them on every call.
* No remote reference is fetched unless a loader is set.
* The decorator's handler receives the payload decoded into its own type,
  and the decorated handler takes the raw event.
* `Validate` returns a new value with the defaults; it never changes the
  caller's data in place.
* Every envelope failure is a `*jmespath.EnvelopeError`; Python wraps only
  some of them.
* A schema of 2019-09 or 2020-12 follows its own draft's rules.
* When several values break the schema, the reported one is the first by
  location in the data, which can differ from Python's keyword order.
* A tuple default is never appended at the wrong index after a missing
  position without a default.
* Numbers keep their full precision.
