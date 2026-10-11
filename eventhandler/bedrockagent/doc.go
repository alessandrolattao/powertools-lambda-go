// Package bedrockagent serves the action groups of Amazon Bedrock agents:
// the Go side of Powertools for AWS Lambda (Python)'s BedrockAgentResolver
// and BedrockAgentFunctionResolver.
//
// # Action groups defined with an OpenAPI schema
//
// The agent calls the API operations the schema describes. Write them as
// typed operations of an eventhandler.Router, each with a description (the
// agent reads it to choose the operation), and adapt the router with
// Handler:
//
//	type WeatherInput struct {
//		City string `query:"city" required:"true" description:"The city"`
//	}
//
//	app := eventhandler.New()
//	eventhandler.Get(app, "/weather", getWeather,
//		eventhandler.Description("Current weather of a city"))
//	lambda.Start(bedrockagent.Handler(app))
//
// The same router writes the schema to upload: OpenAPIJSON returns it in
// OpenAPI 3.0.3, the version agents accept, with the title "Powertools API"
// unless the router sets one. Request validation works as on any router: an
// invalid call answers 422 with the details, which the agent reads. Set the
// session attributes, the prompt session attributes and the knowledge base
// configuration of the response with SetSessionAttributes,
// SetPromptSessionAttributes and SetKnowledgeBasesConfiguration, and read the
// event with EventFromContext. RequireConfirmation makes the agent ask the
// user before calling an operation.
//
// # Action groups defined with function details
//
// The agent calls functions by name with typed parameters. Register a tool
// per function and hand Functions.Handle to lambda.Start:
//
//	fns := bedrockagent.NewFunctions()
//	bedrockagent.Tool(fns, "get_weather", func(ctx context.Context, in struct {
//		City string `json:"city"`
//	}) (string, error) {
//		return "Sunny in " + in.City, nil
//	})
//	lambda.Start(fns.Handle)
//
// # Differences from Powertools Python
//
//   - A router is the resolver: there is no separate resolver type, and the
//     OpenAPI document comes from the router (OpenAPIJSON(app)).
//   - Session attributes of an OpenAPI action group are set through the
//     request context, since the typed operation's response is the HTTP
//     one; a function tool returns a Result.
//   - Body properties are converted to the JSON type the agent declares
//     before validation; Python passes strings for Pydantic to coerce.
//   - A tool returning a string sends it as it is; Python JSON-quotes it
//     ("\"hello\""), unlike its HTTP resolvers.
//   - Registering a tool name twice panics; Python warns and replaces it.
//   - The error body names the Go type of the error ("Error: errorString:
//     ..."), since Go errors have no class.
//   - Operations with security requirements are rejected by OpenAPI, where
//     Python silently drops them.
//
// # Environment variables
//
// POWERTOOLS_DEV turns on the router's debug mode; POWERTOOLS_DEBUG shows
// what the package does in the debug log.
package bedrockagent
