# Data validation and OpenAPI

Typed operations bind and validate requests from Go structs; the same
structs describe the operations in an OpenAPI document, served by the
Swagger UI and merged across functions. This page is the Go port of the
data validation section of the [REST API event
handler](https://docs.powertools.aws.dev/lambda/python/latest/core/event_handler/api_gateway/#data-validation)
and of the [OpenAPI
page](https://docs.powertools.aws.dev/lambda/python/latest/core/event_handler/openapi/)
of Powertools for AWS Lambda (Python). Routing, adapters and middlewares are
on the [REST API page](eventhandler.md).

## Key features

* Input structs whose tags bind path, query, header and cookie parameters,
  form fields, uploaded files and the JSON body.
* Constraints in the tags of
  [`github.com/swaggest/jsonschema-go`](https://github.com/swaggest/jsonschema-go):
  one schema per operation validates the request, describes it in the
  OpenAPI document and, on request, validates the response. They cannot
  drift apart.
* Python's validation errors: 422 with
  `{"statusCode":422,"detail":[{"loc":[...],"type":"..."}]}`, 415 for an
  unsupported content type, Pydantic's error types.
* OpenAPI 3.1 document (3.0.3 for Bedrock agents) built with
  [`github.com/swaggest/openapi-go`](https://github.com/swaggest/openapi-go):
  metadata, servers, tags, security schemes, extensions.
* Swagger UI 5.31.0, embedded.
* OpenAPI merge for APIs whose routes live in several functions.

## Data validation

### Input struct

`Register` and its shortcuts (`Get`, `Post`, `Put`, `Patch`, `Delete`,
`Head`) take a function `func(ctx context.Context, in In) (Out, error)`.
Each field of `In` says where its value comes from:

| Tag or name | Source | Python |
|---|---|---|
| `path:"id"` | path wildcard `{id}` (always required) | `Path()` |
| `query:"limit"` | query string parameter | `Query()` |
| `header:"X-Trace-Id"` | header, matched case-insensitively | `Header()` |
| `cookie:"session"` | cookie | `Cookie()` |
| `formData:"name"` | field of an urlencoded or multipart form | `Form()` |
| `formData:"file"` of type `*multipart.FileHeader` | uploaded file | `File()`, `UploadFile` |
| field named `Body` | JSON request body (`[]byte`: the raw body) | `Body()` |

Embedded structs contribute their fields, the way a Pydantic model expands
into query parameters:

```go
type Paging struct {
	Limit  int  `query:"limit" default:"10" minimum:"1" maximum:"100"`
	Offset *int `query:"offset"`
}

type ListTodos struct {
	Paging

	Completed *bool    `query:"completed"`
	Tags      []string `query:"tag"`
	TraceID   string   `header:"X-Trace-Id" required:"true"`
	Session   string   `cookie:"session"`
}

eventhandler.Get(app, "/todos", func(ctx context.Context, in ListTodos) ([]Todo, error) {
	return store.List(ctx, in.Limit, in.Tags)
})
```

Parameters are converted to the field type: `string`, `bool` (Python's
truth values: `true`, `1`, `yes`, `on`...), integers, floats, types
implementing `encoding.TextUnmarshaler` (`time.Time`, `netip.Addr`),
pointers to them (nil when the parameter is absent) and slices of them (every
value of a repeated parameter; header values are split on commas). A
missing parameter keeps its zero value, or takes its `default`; with
`required:"true"` it is an error.

The body is decoded from JSON with `encoding/json/v2`, after validation: a
non-pointer `Body` is required, a pointer one is optional. Its type is any Go
type, a struct, a slice, a map:

```go
type Todo struct {
	ID    int      `json:"id"`
	Title string   `json:"title" required:"true" minLength:"1" maxLength:"200"`
	Tags  []string `json:"tags,omitempty" maxItems:"5"`
}

eventhandler.Post(app, "/todos", func(ctx context.Context, in struct{ Body Todo }) (Todo, error) {
	return store.Create(ctx, in.Body)
})
```

A request body is read according to its `Content-Type`: JSON (or no content
type) for a `Body`, a form for `formData` fields; any other answers 415.
Files are `*multipart.FileHeader`s, opened with `Open`:

```go
type Upload struct {
	Name string                `formData:"name" required:"true"`
	File *multipart.FileHeader `formData:"file" required:"true"`
}
```

An API Gateway REST API needs `multipart/form-data` among its binary media
types for uploads to arrive intact.

### Constraints

| Tag | JSON Schema | Python |
|---|---|---|
| `required:"true"` | `required` | no default |
| `minimum`, `maximum` | `minimum`, `maximum` | `ge`, `le` |
| `exclusiveMinimum`, `exclusiveMaximum` | same | `gt`, `lt` |
| `multipleOf` | same | `multiple_of` |
| `minLength`, `maxLength` | same | `min_length`, `max_length` |
| `pattern` | same (RE2 syntax) | `pattern` |
| `enum:"a,b"` | `enum` | `Literal`, `Enum` |
| `format:"email"` (`uuid`, `date-time`, `date`, `uri`, `ipv4`...) | `format`, asserted | `EmailStr`, `UUID`... |
| `minItems`, `maxItems`, `uniqueItems` | same | `min_length` on lists |
| `default` | `default` | default value |
| `description`, `title`, `example`, `examples`, `deprecated` | same | `description`... |

Types can shape their own schema with jsonschema-go's interfaces
(`PrepareJSONSchema`, `JSONSchema`, `JSONSchemaOneOf` for discriminated
unions, decoded by the type's `UnmarshalJSON`).

The schemas compile when the operation registers, with a validator built
into the event handler: a general JSON Schema engine compiles the
meta-schemas of every draft when it loads, milliseconds of every cold start.
It implements the keywords the tags generate: `type` (with `null`), `enum`,
`const`, `format`, `$ref` within the document, `properties`, `required`,
`additionalProperties`, `minProperties`, `maxProperties`, `items`,
`minItems`, `maxItems`, `uniqueItems`, `minLength`, `maxLength` (in
characters), `pattern`, `minimum`, `maximum`, `exclusiveMinimum`,
`exclusiveMaximum`, `multipleOf`, `allOf`, `anyOf`, `oneOf`. Its formats are
`date-time`, `date`, `time`, `duration`, `email`, `hostname`, `ipv4`,
`ipv6`, `uri`, `uri-reference`, `iri`, `iri-reference`, `uuid` and `regex`;
`int32`, `int64`, `float`, `double`, `byte`, `binary` and `password`
describe without constraining. A schema with anything else (`not`,
`if`/`then`, `patternProperties`, another format), which only a type's own
schema methods can produce, makes the registration panic instead of being
half enforced.

### Handling validation errors

A request that does not match its input answers 422 with Python's body, the
errors in Python's order (path, query, header, cookie, body):

```json
{"statusCode":422,"detail":[{"loc":["path","id"],"type":"greater_than_equal"},{"loc":["query","limit"],"type":"int_parsing"}]}
```

`loc` is where: the location, the name, then the field path, with array
indexes as numbers (`["body","tags",1]`). `type` is Pydantic's error code:

| Failure | Type |
|---|---|
| missing required value | `missing` |
| value that does not parse | `int_parsing`, `float_parsing`, `bool_parsing`, `datetime_parsing` |
| JSON value of the wrong type | `string_type`, `int_type`, `int_from_float`, `float_type`, `bool_type`, `list_type`, `model_attributes_type` |
| `minLength`, `maxLength` | `string_too_short`, `string_too_long` |
| `minimum`, `exclusiveMinimum`, `maximum`, `exclusiveMaximum` | `greater_than_equal`, `greater_than`, `less_than_equal`, `less_than` |
| `pattern`, `enum`, `multipleOf` | `string_pattern_mismatch`, `enum`, `multiple_of` |
| `minItems`, `maxItems` | `too_short`, `too_long` |
| formats | `uuid_parsing`, `date_parsing`, `url_parsing`, `ip_v4_address`, `value_error`... |
| body that is not JSON | `json_invalid`, at `["body", <byte offset>]`, reported alone |
| bad form or multipart body | `form_invalid`, `multipart_invalid` |

The messages and the values stay out of the response, as in Python; an error
handler gets them in the `*RequestValidationError`:

```go
eventhandler.OnError(app, func(w http.ResponseWriter, r *http.Request, err *eventhandler.RequestValidationError) error {
	slog.WarnContext(r.Context(), "invalid request", "errors", err.Error())
	return eventhandler.WriteJSON(w, r, http.StatusBadRequest, map[string]int{"errors": len(err.Errors)})
})
```

A body of an unsupported content type answers 415 with
`{"statusCode":415,"detail":[{"loc":["body"],"type":"unsupported_content_type"}]}`
(`*UnsupportedContentTypeError`).

### Validating responses

Go types already shape the output; constraints (`minLength`...) are checked
when asked. `WithResponseValidation(status)` validates the responses of every
typed operation, `ResponseValidation(status)` those of one route, and a
response that does not conform answers that status with
`{"statusCode":...,"detail":[{"loc":["response",...],"type":"..."}]}`
(`*ResponseValidationError`).

### Responses

`Out` is written as JSON with status 200 (`StatusCode` changes it), or as a
`Response[T]` that chooses its status, headers, cookies and compression. A
`[]byte` body is written as it is (`application/octet-stream` unless a
header says otherwise); 204 and 304 responses have no body.

## OpenAPI

`app.OpenAPIJSON()` returns the OpenAPI 3.1 document of the router's routes,
`app.OpenAPI()` the same as a `*openapi31.Spec`. Typed operations are
described from their input and output types, plain handlers by their path
parameters. Write the document at build time:

```go
func main() {
	if len(os.Args) > 1 && os.Args[1] == "openapi" {
		doc, err := app.OpenAPIJSON()
		if err != nil {
			panic(err)
		}
		_ = os.WriteFile("openapi.json", doc, 0o600)
		return
	}
	lambda.Start(eventhandler.APIGatewayREST(app))
}
```

### Customizing metadata

```go
app := eventhandler.New(eventhandler.WithOpenAPI(eventhandler.OpenAPIConfig{
	Title:       "Todos API",
	Version:     "2.0.0",
	Summary:     "Manage todos",
	Description: "An API to **manage** todos.",
	Contact:     &eventhandler.Contact{Name: "Team", Email: "team@example.com"},
	License:     &eventhandler.License{Name: "MIT", Identifier: "MIT"},
	Servers:     []eventhandler.Server{{URL: "https://api.example.com"}},
	Tags:        []eventhandler.Tag{{Name: "todos", Description: "Todo items"}},
	ExternalDocs: &eventhandler.ExternalDocs{URL: "https://example.com/docs"},
}))
```

The title defaults to "Powertools for AWS Lambda (Go) API", the version to
1.0.0, the servers to `/`. `OpenAPIJSON` takes options that adjust one
document: `app.OpenAPIJSON(func(c *eventhandler.OpenAPIConfig) { c.Version = "2.1.0" })`.

### Customizing operations

| Route option | OpenAPI | Python |
|---|---|---|
| `Summary` | `summary` (default "METHOD /path") | `summary` |
| `Description` | `description` | `description` |
| `Tags` | `tags` | `tags` |
| `OperationID` | `operationId` (default: function name, path, method) | `operation_id` |
| `Deprecated` | `deprecated` | `deprecated` |
| `ExcludeFromSchema` | route left out | `include_in_schema=False` |
| `StatusCode` | status of the successful response | `status_code` |
| `ResponseDescription` | its description ("Successful Response") | `response_description` |
| `ResponseModel(status, description, model)` | documented responses | `responses` |
| `Security` | `security` | `security` |
| `Extension("x-...", value)` | operation extension | `openapi_extensions` |

```go
eventhandler.Get(app, "/todos/{id}", getTodo,
	eventhandler.Tags("todos"), eventhandler.Summary("Get a todo"),
	eventhandler.ResponseModel(http.StatusOK, "The todo", nil),
	eventhandler.ResponseModel(http.StatusNotFound, "No such todo", ErrorBody{}))
```

Typed operations document a 422 response (`HTTPValidationError`), and the
response validation status when one is set. Components are named after the
Go types. Regular expression routes and routes without a method cannot be
described and are left out.

### Security schemes

```go
app := eventhandler.New(eventhandler.WithOpenAPI(eventhandler.OpenAPIConfig{
	SecuritySchemes: map[string]eventhandler.SecurityScheme{
		"bearer": eventhandler.HTTPBearerScheme("JWT"),
		"apiKey": eventhandler.APIKeyScheme("X-Api-Key", "header"),
		"oauth": eventhandler.OAuth2Scheme(eventhandler.OAuthFlows{AuthorizationCode: &eventhandler.OAuthFlow{
			AuthorizationURL: "https://auth.example.com/authorize",
			TokenURL:         "https://auth.example.com/token",
			Scopes:           map[string]string{"todos:write": "Write todos"},
		}}),
	},
	Security: []eventhandler.SecurityRequirement{{"bearer": nil}},
}))
eventhandler.Post(app, "/todos", createTodo, eventhandler.Security(eventhandler.SecurityRequirement{"oauth": {"todos:write"}}))
```

`HTTPScheme`, `OpenIDConnectScheme` and `MutualTLSScheme` build the others.
A requirement naming an undeclared scheme fails the document with
`ErrSchemaValidation`. Powertools documents security; it does not enforce it:
use an authorizer or a middleware.

### OpenAPI extensions

`OpenAPIConfig.Extensions`, `Server.Extensions`, `SecurityScheme.Extensions`
and the `Extension` route option add `x-` fields; other names fail the
document with `ErrSchemaValidation`.

## Swagger UI

```go
swaggerui.Enable(app)
```

serves the Swagger UI at `GET /swagger`: `?format=json` answers the document,
`?format=oauth2-redirect` the OAuth2 redirect page. The page inlines Swagger
UI 5.31.0, the version Powertools Python bundles (about 1.7 MB, embedded only
in binaries that import `eventhandler/swaggerui`); the document targets the
API Gateway stage, so that "Try it out" works behind a stage URL.

| Option | Effect | Python |
|---|---|---|
| `WithPath("/docs")` | where the UI is served | `path` |
| `WithBaseURL(url)` | link `{url}/swagger-ui-bundle.min.js` and `{url}/swagger-ui.min.css` instead of inlining them | `swagger_base_url` |
| `WithCSSURL`, `WithJSURL` | link those assets from anywhere | |
| `WithPersistAuthorization()` | keep the authorization across reloads | `persist_authorization` |
| `WithOAuth2(cfg)` | OAuth2 configuration of the UI | `oauth2_config` |
| `WithMiddlewares(...)` | protect the route, public otherwise | `middlewares` |
| `WithCompression()` | gzip the page | `compress` |
| `WithOpenAPI(...)` | adjust the document (servers, title...) | `title`, `servers`... |
| `WithDocument(fn)` | serve another document, a merged one | `configure_openapi_merge` |

An OAuth2 client secret is visible to every visitor: `Enable` panics with
`ErrClientSecret` unless `POWERTOOLS_DEV` is on, as Python refuses it.

## OpenAPI merge

An API whose routes live in several functions has several documents.
`MergeOpenAPI` merges them, with the document-level fields of a
configuration:

```go
merged, err := eventhandler.MergeOpenAPI(eventhandler.OpenAPIConfig{Title: "Shop API"},
	eventhandler.ConflictWarn, ordersDoc, usersDoc)
```

Paths merge per method; components of the same name are replaced by the
later document's; the configured tags come first. When two documents define
the same operation, `ConflictWarn` (the default) keeps the first and warns,
`ConflictError` fails with `ErrMergeConflict`, `ConflictFirst` keeps the
first silently, `ConflictLast` keeps the last.

Python discovers the handlers by scanning source files and importing them.
A Go function's routes live in its binary: have each function package export
the function that registers its routes, and let a build step import them
all, render each document and merge them. The program in
[`eventhandler/examples/openapimerge`](../eventhandler/examples/openapimerge/main.go)
does so:

```shell
go run ./eventhandler/examples/openapimerge openapi.json
```

A function serving the documentation shows the merged document with
`swaggerui.WithDocument`.

## Testing your code

The 422 and 415 bodies are deterministic: compare them as strings in tests.
Validate the generated document against the OpenAPI specification's own JSON
Schema, as this package's tests do, to catch a malformed description before
it ships.

## Differences from Powertools Python

* Parameters are bound by struct tags, not inferred from a signature;
  required values say so with `required:"true"`.
* JSON bodies decode strictly: `"7"` is not an integer. Pydantic coerces it.
* A string output is written as a JSON string; Python sends `str` bodies as
  they are.
* Response validation is off by default, and its failures are
  `*ResponseValidationError`s (422 unless configured); Python validates
  responses whenever validation is on and reports failures as request
  validation errors.
* Bodies of GET, HEAD and DELETE operations are bound and validated but not
  documented. Parameter `openapi_examples` are not supported: use the
  `example` and `examples` tags.
* The validation error schemas list `statusCode` and do not require `msg`,
  matching the bodies actually sent.
* The merge takes documents, not source directories.
