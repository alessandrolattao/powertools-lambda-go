# Amazon Bedrock Agents

The `eventhandler/bedrockagent` package serves the action groups of Amazon
Bedrock agents, defined with an OpenAPI schema or with function details. It
is the Go port of the [Powertools for AWS Lambda (Python) Bedrock Agents
event handler](https://docs.powertools.aws.dev/lambda/python/latest/core/event_handler/bedrock_agents/).

## Key features

* OpenAPI action groups served by an `eventhandler.Router`: the same typed
  operations, validation and error handling as an HTTP API.
* The OpenAPI 3.0.3 schema to upload, generated from the code.
* Function action groups with typed parameters.
* Session attributes, prompt session attributes and knowledge base
  configuration in the response.

## Terminology

An **action group** is the set of actions an agent can take; the agent
calls the Lambda function of the group with the action it chose and the
parameters it extracted from the conversation. A group is defined with an
**OpenAPI schema** (the agent calls API operations) or with **function
details** (the agent calls functions by name).

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go
```

### Your first agent

Write the actions as typed operations, each with a description: the agent
reads it to decide when to call the operation.

```go
type WeatherInput struct {
	City string `query:"city" required:"true" description:"The city to look up"`
}

type Weather struct {
	City        string  `json:"city"`
	Temperature float64 `json:"temperature"`
}

func newApp() *eventhandler.Router {
	app := eventhandler.New()
	eventhandler.Get(app, "/weather", func(ctx context.Context, in WeatherInput) (Weather, error) {
		return Weather{City: in.City, Temperature: 21.5}, nil
	}, eventhandler.Description("Current weather of a city"))
	return app
}

func main() {
	lambda.Start(bedrockagent.Handler(newApp()))
}
```

The handler turns the agent's call into the HTTP request of the operation:
its method and API path, the path parameters filled in from the parameters
of the same name, every parameter a query parameter too (never split on
commas), the `application/json` body properties a JSON object whose values
have the type the agent declares (integer, number, boolean, array). The
response carries the status and the body under its content type:

```json
{
  "messageVersion": "1.0",
  "response": {
    "actionGroup": "weather",
    "apiPath": "/weather",
    "httpMethod": "GET",
    "httpStatusCode": 200,
    "responseBody": {"application/json": {"body": "{\"city\":\"Rome\",\"temperature\":21.5}"}}
  }
}
```

### Accessing custom request fields

`bedrockagent.EventFromContext(ctx)` returns the event: session id, input
text, agent, session attributes.

## OpenAPI-based actions

### Generating OpenAPI schemas

`bedrockagent.OpenAPIJSON(app)` returns the document in OpenAPI 3.0.3, the
version agents accept, titled "Powertools API" unless the router names it.
Write it at build time and upload it as the group's API schema:

```go
if len(os.Args) > 1 && os.Args[1] == "schema" {
	doc, err := bedrockagent.OpenAPIJSON(newApp())
	if err != nil {
		panic(err)
	}
	_ = os.WriteFile("schema.json", doc, 0o600)
	return
}
```

Every operation needs a description (`ErrMissingDescription`), and none may
set security requirements (`ErrSecurityUnsupported`).

### Crafting effective OpenAPI schemas

The agent chooses operations and fills parameters from their descriptions:
describe each operation, parameter and body field (`description` tags), name
them plainly, and keep the parameters few.

### Validating input and output

Requests are validated as on any router: a call that does not match the
operation answers 422 with the details, which the agent reads and may
correct. `eventhandler.WithResponseValidation` validates the responses too.

### Advanced

#### Additional metadata

`eventhandler.WithOpenAPI` sets the title, version, description and servers
of the document; the route options (`Summary`, `Description`, `Tags`,
`OperationID`, `ResponseModel`...) describe the operations.

#### Enabling user confirmation

`bedrockagent.RequireConfirmation()` asks the agent to confirm with the user
before calling the operation (`x-requireConfirmation: ENABLED`):

```go
eventhandler.Post(app, "/orders", placeOrder,
	eventhandler.Description("Place an order"), bedrockagent.RequireConfirmation())
```

#### OpenAPI-based responses

The operation's output is the body; set the attributes of the agent
response from the operation's context:

```go
bedrockagent.SetSessionAttributes(ctx, map[string]string{"lastCity": in.City})
bedrockagent.SetPromptSessionAttributes(ctx, map[string]string{"unit": "celsius"})
bedrockagent.SetKnowledgeBasesConfiguration(ctx, []bedrockagent.KnowledgeBaseConfiguration{{KnowledgeBaseID: "KB1"}})
```

Python returns them in a `BedrockResponse`; in Go the operation's response is
the HTTP one, so they travel through the context. Only the attributes set
are sent.

#### Bedrock requests under the hood

The router sees an ordinary request: middlewares, error handlers, not found
handler and debug mode work as for an HTTP API. An error no handler takes
fails the invocation, as in Python.

### Testing your code

Call the handler with an event:

```go
resp, err := bedrockagent.Handler(newApp())(ctx, eventsource.BedrockAgentEvent{
	ActionGroup: "weather", HTTPMethod: "GET", APIPath: "/weather",
	Parameters: []eventsource.BedrockAgentProperty{{Name: "city", Type: "string", Value: "Rome"}},
})
```

## Function-based actions

Register a tool per function and hand `Handle` to `lambda.Start`:

```go
type forecastInput struct {
	City string `json:"city"`
	Days int    `json:"days"`
}

func forecast(_ context.Context, in forecastInput) (string, error) {
	return fmt.Sprintf("Sunny in %s for %d days", in.City, in.Days), nil
}

func main() {
	fns := bedrockagent.NewFunctions()
	bedrockagent.Tool(fns, "get_weather", forecast, bedrockagent.Description("Weather forecast of a city"))
	lambda.Start(fns.Handle)
}
```

Parameters are converted as Python converts them (integers and numbers
parsed, booleans true for "true", arrays decoded when they are JSON) and
decoded into the input by their `json` names: unknown parameters are ignored,
missing ones stay zero. A string output is the body as it is, anything else
is JSON. An error or an unknown function never fails the invocation: the
body is `Error: <type>: <message>`.

### Function-based responses

A tool that sets more than the body returns a `Result`:

```go
return bedrockagent.Result[string]{
	Body:              "Booked",
	SessionAttributes: map[string]string{"booking": "B42"},
	ResponseState:     bedrockagent.Reprompt,
}, nil
```

`ResponseState` is empty, `Failure` or `Reprompt`. Session attributes default
to the event's.

## Differences from Powertools Python

* A router is the resolver; the schema comes from `OpenAPIJSON(app)`.
* Response attributes of OpenAPI groups are set through the context.
* Body properties are converted to their declared JSON type before
  validation; Python passes strings for Pydantic to coerce.
* A tool's string output is not JSON-quoted (Python sends `"\"hello\""`).
* Registering a tool name twice panics; Python warns and replaces it.
* The error body names the Go type of the error.
* Operations with security requirements are rejected; Python drops them.
