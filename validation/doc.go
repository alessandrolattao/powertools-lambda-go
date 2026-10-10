// Package validation validates events and responses against JSON Schemas,
// optionally after unwrapping the payload of an event source with a JMESPath
// envelope: the Go side of Powertools for AWS Lambda (Python)'s validation
// utility.
//
// It is schema-document validation. To decode events into typed Go models
// with a Validate method of their own, see the parser package; the two
// combine.
//
// # Validating in the handler
//
// Validate checks an event against a schema and returns the validated data,
// with the schema defaults filled in. A *SchemaValidationError says what is
// wrong, in the words of Powertools Python:
//
//	var orderSchema = validation.MustCompile(orderSchemaJSON)
//
//	func handle(ctx context.Context, event jsontext.Value) (events.APIGatewayProxyResponse, error) {
//		if _, err := validation.Validate(event, orderSchema, validation.WithEnvelope(validation.APIGatewayREST)); err != nil {
//			return events.APIGatewayProxyResponse{StatusCode: 400, Body: err.Error()}, nil
//		}
//		...
//	}
//
// ValidateAs validates and decodes the result into a Go type.
//
// # Validating with a decorator
//
// Validator wraps a handler: it validates the event before the handler runs
// and the response after it, as Python's @validator does. The returned
// handler takes the raw event; the wrapped one receives the payload the
// envelope selects, defaults included, decoded into its own event type:
//
//	lambda.Start(validation.Validator(handle,
//		validation.Inbound(orderSchema, validation.WithEnvelope(validation.EventBridge)),
//		validation.Outbound(responseSchema),
//	))
//
// # Schemas
//
// A schema is raw JSON ([]byte, jsontext.Value, or a string such as one
// read with go:embed), a value that marshals to JSON (a map[string]any), or
// a *Schema from Compile. Compile once, at package level or in init:
// Validate also accepts schema documents and caches what it compiles, except
// when the options carry functions (custom formats, loaders, a regexp
// engine), which Go cannot compare; compile those schemas with Compile.
//
// The "$schema" keyword picks the draft: 4, 6, 7, 2019-09 and 2020-12 are
// supported. A schema without it is read as DefaultDraft, Draft 7, which is
// what Powertools Python applies (fastjsonschema's fallback generator is
// labelled 2019-09 but implements draft-07). WithDefaultDraft changes it.
//
// Formats are asserted for every draft, as in Python; WithFormatAssertion
// turns them off. WithFormat and WithFormatRegexp add custom formats, which
// apply to strings only.
//
// # References
//
// A "$ref" to a part of the same schema always resolves. Other documents
// come from WithResource, which preloads a document under a URL, or from a
// loader for the scheme of their URL: WithLoader (Python's handlers, with a
// context) or WithHTTPLoader. Nothing is fetched otherwise: a reference
// without a document or a loader fails the compilation with ErrNoLoader.
// Give a schema that references documents by relative URLs an "$id", the
// base they resolve against.
//
// # Envelopes
//
// WithEnvelope takes a JMESPath expression, which can call the Powertools
// functions powertools_json, powertools_base64 and powertools_base64_gzip
// (see the jmespath package), and the custom functions given to it, next to
// the Powertools ones (Python's jmespath_options replace them). The
// constants of this package are Python's built-in envelopes. A failing envelope is a *jmespath.EnvelopeError,
// matched by errors.Is(err, jmespath.ErrInvalidEnvelope).
//
// # Errors
//
//   - *SchemaValidationError (errors.Is ErrSchemaValidation): the data breaks
//     the schema. Its fields are those of Python's exception: Message,
//     ValidationMessage, Name, Path, Value, Definition, Rule and
//     RuleDefinition. It unwraps to the engine's *jsonschema.ValidationError,
//     whose BasicOutput lists every failure.
//   - *InvalidSchemaFormatError (errors.Is ErrInvalidSchema): the schema does
//     not compile.
//   - ErrInvalidEvent: the event is not JSON.
//
// # Environment variables
//
// None. POWERTOOLS_DEBUG shows what the package does in the debug log.
//
// # Differences from Powertools Python
//
//   - Schemas compile once and are reused across invocations; Python compiles
//     on every call. A schema error surfaces when Compile runs, or, for
//     Validator, on the invocations until the schema compiles.
//   - No remote "$ref" is fetched unless a loader is set; fastjsonschema
//     fetches http and https URLs by default.
//   - Validate returns the data with defaults as a new value; it never
//     changes the caller's data in place. The decorator hands the defaults
//     to the handler through its decoded event, and fills those of the
//     outbound schema into the response, as Python's in-place update does.
//   - The decorator's handler receives the payload decoded into its own
//     type, and the returned handler takes the raw event (jsontext.Value).
//   - Patterns are Go regular expressions (RE2): no lookarounds or
//     backreferences unless WithRegexpEngine plugs in another engine. A
//     format pattern is anchored at its start only, like Python's re.match.
//   - A schema of 2019-09 or 2020-12 is validated by that draft's rules;
//     fastjsonschema applies draft-07 rules to them.
//   - Every envelope failure is a *jmespath.EnvelopeError; Python wraps only
//     lexer, type and Unicode errors.
//   - When several values break the schema, Python reports the first in its
//     keyword order; Go reports the first by location in the data, then in
//     the schema, which may name a different one of the same failures.
//   - A tuple default is not appended after a missing position that has no
//     default (fastjsonschema shifts it to the wrong index).
//   - Numbers keep their precision: the validated data holds json.Number
//     values. A number selected by an envelope goes through JMESPath as a
//     float64.
package validation
