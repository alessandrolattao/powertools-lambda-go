# REST API

The event handler routes the HTTP requests that reach a Lambda function
through Amazon API Gateway (REST and HTTP APIs), an Application Load
Balancer, a Lambda Function URL or VPC Lattice, and turns what your handlers
write into the response each source expects. It is the Go port of the
[Powertools for AWS Lambda (Python) REST API event
handler](https://docs.powertools.aws.dev/lambda/python/latest/core/event_handler/api_gateway/).

Its data validation, OpenAPI document and Swagger UI have [their own
page](eventhandler-openapi.md); Amazon Bedrock agents [too](bedrock-agents.md).

## Key features

* Lightweight routing on `net/http`: a `Router` is an `http.Handler`, routes
  are `http.Handler`s, middlewares are `func(http.Handler) http.Handler`.
* One router for every source: API Gateway REST API and HTTP API (payload
  1.0 and 2.0), Application Load Balancer, Lambda Function URL, VPC Lattice
  (event structures 1 and 2).
* Typed operations: path, query, header and cookie parameters, forms, files
  and JSON bodies bound into a Go struct and validated, the 422 responses of
  Python included.
* Error handlers by error type, built-in HTTP errors, not found handler.
* CORS with pre-flight, gzip compression, binary responses, cache control,
  cookies.
* OpenAPI 3.1 document, Swagger UI, OpenAPI merge of several functions.
* Local development with `http.ListenAndServe`: no emulator needed.

## Getting started

### Install

```shell
go get github.com/alessandrolattao/powertools-lambda-go
```

### Required resources

The event handler serves the Lambda proxy integrations: an API Gateway REST
API with a `{proxy+}` resource (or one resource per route) using the Lambda
proxy integration, an HTTP API route (`$default` or `ANY /{proxy+}`), an ALB
target group of type Lambda, a Function URL, or a VPC Lattice target group of
type Lambda. Build the function for the `provided.al2023` runtime:

```shell
GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap ./cmd/api
```

### Event resolvers

Create a `Router`, register routes, and hand the router to the adapter of
your event source:

```go
func hello(w http.ResponseWriter, r *http.Request) {
	if err := eventhandler.WriteJSON(w, r, http.StatusOK, map[string]string{"message": "hello " + r.PathValue("name")}); err != nil {
		eventhandler.Error(w, r, err)
	}
}

func main() {
	app := eventhandler.New()
	app.HandleFunc("GET /hello/{name}", hello)
	lambda.Start(eventhandler.APIGatewayREST(app))
}
```

| Source | Adapter | Event | Response | Python |
|---|---|---|---|---|
| API Gateway REST API, HTTP API payload 1.0 | `APIGatewayREST` | `events.APIGatewayProxyRequest` | `events.APIGatewayProxyResponse` | `APIGatewayRestResolver` |
| API Gateway HTTP API payload 2.0 | `APIGatewayHTTP` | `events.APIGatewayV2HTTPRequest` | `events.APIGatewayV2HTTPResponse` | `APIGatewayHttpResolver` |
| Application Load Balancer | `ALB` | `events.ALBTargetGroupRequest` | `events.ALBTargetGroupResponse` | `ALBResolver` |
| Lambda Function URL (buffered) | `FunctionURL` | `events.LambdaFunctionURLRequest` | `events.LambdaFunctionURLResponse` | `LambdaFunctionUrlResolver` |
| VPC Lattice, version 1 | `VPCLattice` | `eventsource.VPCLatticeEvent` | `VPCLatticeResponse` | `VPCLatticeResolver` |
| VPC Lattice, version 2 | `VPCLatticeV2` | `eventsource.VPCLatticeEventV2` | `VPCLatticeResponse` | `VPCLatticeV2Resolver` |

The adapter builds an `*http.Request` from the event: method, path, query
string with every value of repeated parameters, headers, cookies, body
(base64-decoded when the event says so) and the caller's address in
`RemoteAddr`. After your handler, it builds the response the source
expects:

* **REST API**: every header, `Set-Cookie` included, goes in
  `multiValueHeaders`. Routes ignore trailing slashes (`/todos/` matches
  `GET /todos`), as Python's REST resolver does.
* **HTTP API, Function URL**: header values are joined with `", "`; cookies
  go in the `cookies` list. The stage prefix is removed from the path unless
  the stage is `$default`. An HTTP API with payload format 1.0 uses
  `APIGatewayREST`.
* **ALB**: with multi-value headers enabled on the target group, every
  header value and every cookie is sent; without, the last one of each, with
  a warning. Query strings arrive percent-encoded from ALB: the request URL
  keeps them so, and `r.URL.Query()` decodes them.
* **VPC Lattice**: header values are joined with `", "`; Lattice reads one
  cookie, the last `Set-Cookie`. Version 1 folds repeated query parameters
  into one comma-separated value; list parameters of typed operations split
  it.

#### Validate the ALB response size

An ALB rejects Lambda responses larger than 1 MB.
`WithResponseSizeValidation` measures the serialized response and turns a
bigger one into a `*ResponseSizeExceededError`, which an error handler can
answer with a smaller response:

```go
eventhandler.OnError(app, func(w http.ResponseWriter, r *http.Request, _ *eventhandler.ResponseSizeExceededError) error {
	return eventhandler.WriteJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "too big"})
})
lambda.Start(eventhandler.ALB(app, eventhandler.WithResponseSizeValidation()))
```

The handled response is measured again; the invocation fails when it is
still too big or when no handler takes the error.

#### Function URL streaming

`FunctionURL` serves the BUFFERED invoke mode. For RESPONSE_STREAM, serve
the router with aws-lambda-go's `lambdaurl.Start(app)`: it streams what the
router writes.

### Dynamic routes

Patterns use the syntax of `net/http`: a method, then a path whose
`{name}` segments are wildcards. Read their values with `r.PathValue`:

```go
app.HandleFunc("GET /todos/{id}/{status}", func(w http.ResponseWriter, r *http.Request) {
	id, status := r.PathValue("id"), r.PathValue("status")
	// ...
})
```

A wildcard matches one non-empty segment, decoded (`/files/a%20b` gives
`a b`). Matching follows Powertools rather than `http.ServeMux`:

* a pattern matches the whole path; a trailing slash is not a subtree (`{$}`
  is accepted and changes nothing);
* routes without wildcards are tried first, then the others in registration
  order;
* a path registered for another method answers 404, not 405;
* `GET` does not answer `HEAD`;
* nothing redirects (`//`, `/./` are not cleaned up);
* a pattern without a method matches every method;
* registering the same method and path shape twice panics.

#### Catch-all routes

`{name...}` matches the rest of the path:

```go
app.HandleFunc("GET /files/{path...}", serveFile)
```

Python's regular expression rules are `HandleRegexp` routes. The expression
must match the whole path; its named groups are the path values. A route
whose expression has groups is tried with the wildcard routes, one without
with the literal routes:

```go
app.HandleRegexp(http.MethodGet, regexp.MustCompile(`/v\d+/users/(?P<user>\w+)`), getUser)
```

### HTTP methods

The method is part of the pattern (`"POST /todos"`). Typed operations have
shortcuts per method: `eventhandler.Get`, `Post`, `Put`, `Patch`, `Delete`,
`Head`, and `Register` for any pattern.

### Typed operations

`Get` and its siblings register a function that receives its input bound
from the request and returns its output, written as JSON:

```go
type Todo struct {
	ID    int    `json:"id"`
	Title string `json:"title" required:"true" minLength:"1"`
}

type GetTodo struct {
	ID     int    `path:"id" minimum:"1"`
	Fields string `query:"fields" enum:"all,title" default:"all"`
}

eventhandler.Get(app, "/todos/{id}", func(_ context.Context, in GetTodo) (Todo, error) {
	if in.ID == 42 {
		return Todo{}, eventhandler.NotFound("todo 42 not found")
	}
	return Todo{ID: in.ID, Title: "Buy milk"}, nil
})
```

A request that does not match the input answers 422 with the body Python
sends:

```json
{"statusCode":422,"detail":[{"loc":["path","id"],"type":"greater_than_equal"}]}
```

The input struct, the constraints, forms, files and the OpenAPI document
they generate are described in [Data validation and
OpenAPI](eventhandler-openapi.md).

### Accessing request details

A plain handler has the `*http.Request`: `r.URL.Query()`, `r.Header`,
`r.Cookie`, `r.Body`. A typed operation reads it with
`eventhandler.RequestFromContext(ctx)`. The raw Lambda event is in the
request context too, with a getter per source:

```go
e, ok := eventhandler.APIGatewayRESTEvent(r.Context()) // events.APIGatewayProxyRequest
claims := e.RequestContext.Authorizer["claims"]
```

`APIGatewayHTTPEvent`, `ALBEvent`, `FunctionURLEvent`, `VPCLatticeEvent` and
`VPCLatticeV2Event` return the others, `lambdacontext.FromContext` the
Lambda context.

### Handling not found routes

A request no route matches answers 404 with
`{"statusCode":404,"message":"Not found"}`. `HandleNotFound` replaces it; it
also answers the `*ServiceError`s with status 404 that handlers return:

```go
app.HandleNotFound(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_ = eventhandler.WriteJSON(w, r, http.StatusNotFound, map[string]string{"error": "no such route: " + r.URL.Path})
}))
```

### Exception handling

`OnError` registers a handler per error type. A typed operation that returns
an error, a plain handler or a middleware that calls `eventhandler.Error`,
reach it:

```go
var errOutOfStock = errors.New("out of stock")

eventhandler.OnError(app, func(w http.ResponseWriter, r *http.Request, err error) error {
	if errors.Is(err, errOutOfStock) {
		return eventhandler.WriteJSON(w, r, http.StatusConflict, map[string]string{"error": err.Error()})
	}
	return err // the built-in response, or a failed invocation
})
```

Python picks the handler of the most specific exception class. Go errors
have no class hierarchy, so the router looks for a handler in this order:

1. a 404 `*ServiceError` goes to the `HandleNotFound` handler, if any;
2. walking the error tree from the outermost error (`Unwrap`, depth first),
   the first error whose concrete type has a handler wins;
3. otherwise the handlers of interface types, in registration order, the
   first one `errors.As` matches; a handler for `error` itself (Python's
   handler for `Exception`) comes last.

A handler that returns an error hands it to the built-in responses. An error
nothing answers fails the Lambda invocation, as an unhandled exception does
in Python (API Gateway then answers 502); a local server answers 500. Panics
are not recovered: aws-lambda-go reports them.

### Raising HTTP errors

Return a `*ServiceError`, or pass it to `eventhandler.Error`, to answer with
its status and `{"statusCode": ..., "message": ...}`:

| Constructor | Status | Python |
|---|---|---|
| `BadRequest(msg)` | 400 | `BadRequestError` |
| `Unauthorized(msg)` | 401 | `UnauthorizedError` |
| `Forbidden(msg)` | 403 | `ForbiddenError` |
| `NotFound(msg)` | 404, "Not found" by default | `NotFoundError` |
| `RequestTimeout(msg)` | 408 | `RequestTimeoutError` |
| `RequestEntityTooLarge(msg)` | 413 | `RequestEntityTooLargeError` |
| `InternalServerError(msg)` | 500 | `InternalServerError` |
| `ServiceUnavailable(msg)` | 503 | `ServiceUnavailableError` |
| `NewServiceError(status, msg)` | any, message of any JSON type | `ServiceError` |

`errors.Is(err, eventhandler.ErrNotFound)` matches every 404, and the other
`ErrXxx` sentinels their status.

### Enabling Swagger UI

```go
swaggerui.Enable(app)
```

serves the Swagger UI of the router's OpenAPI document at `GET /swagger`.
See [Data validation and OpenAPI](eventhandler-openapi.md#swagger-ui).

### Custom domain API mappings

An API mapped under a base path of a custom domain receives
`/payment/todos` for the route `/todos`. Strip the prefix before routing:

```go
app := eventhandler.New(eventhandler.WithStripPrefixes("/payment"))
```

A path equal to a prefix becomes `/`. `WithStripPrefixRegexps` removes every
match of regular expressions instead (`^/v[123]+/(dev|stg)`), as Python's
compiled patterns do.

## Advanced

### CORS

```go
app := eventhandler.New(eventhandler.WithCORS(eventhandler.CORSConfig{
	AllowOrigin:      "https://www.example.com",
	ExtraOrigins:     []string{"https://dev.example.com"},
	ExposeHeaders:    []string{"X-Request-Id"},
	AllowHeaders:     []string{"X-Custom"},
	MaxAge:           new(300),
	AllowCredentials: true,
}))
app.Handle("GET /public", publicHandler, eventhandler.CORS(false))
```

A request whose `Origin` is allowed gets `Access-Control-Allow-Origin` (the
origin itself, or `*` when any origin is allowed), the allowed headers
(always `Authorization`, `Content-Type`, `X-Amz-Date`, `X-Api-Key`,
`X-Amz-Security-Token`, plus yours), the exposed headers, the max age and,
for a specific origin, the credentials flag. Unless any origin is allowed,
every response carries `Vary: Origin`. A route turns
CORS off with `CORS(false)`, or on in a router without `WithCORS` with
`CORS(true)`.

#### Pre-flight

An `OPTIONS` request no route matches answers 204 with
`Access-Control-Allow-Methods`: the methods of the routes with CORS on, plus
`OPTIONS`.

#### Defaults

`CORSConfig{}` allows any origin (`*`): set `AllowOrigin` in production.

### Middleware

Middlewares are `net/http` middlewares. `Use` adds them to every request,
matched or not (pre-flight and not found requests included);
`Middlewares` adds them to one route, after the router's:

```go
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get("X-Amzn-Trace-Id"))
		next.ServeHTTP(w, r)
	})
}

app.Use(requestID)
app.Handle("POST /todos", createTodo, eventhandler.Middlewares(audit))
```

The first middleware runs first on the way in and last on the way out.

#### Returning early

A middleware short-circuits the request by answering without calling
`next`, or by calling `eventhandler.Error`:

```go
app.Use(func(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			eventhandler.Error(w, r, eventhandler.Unauthorized("Unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
})
```

#### Handling exceptions

Errors are answered where they happen, through the error handlers: the
middlewares around them see the error response and can change it after
`next`. In Python the exception skips their "after" logic.

#### Schema validation middleware

`schemavalidation.Middleware`, in the `eventhandler/schemavalidation`
package, validates the JSON body of every request (and, with an outbound
schema, of every response) against a JSON Schema with the
[validation](validation.md) utility: a request that breaks it answers 400
`Bad Request: <message>`.

```go
app.Handle("POST /users", createUser,
	eventhandler.Middlewares(schemavalidation.Middleware(userSchema, nil)))
```

It is a package of its own so that functions that do not use it do not load
a general JSON Schema engine, whose initialization costs milliseconds of
every cold start. Typed operations validate their input without it.

#### Sharing contextual data

Python's `append_context` and `Depends()` become request context values: a
middleware stores a value with `context.WithValue` and passes the derived
request on, the handler reads it with a typed getter. Dependencies of the
handlers themselves (clients, stores) are fields of a struct whose methods
are the handlers, or closures, which tests replace freely.

### Fine-grained responses

A typed operation that chooses its status, headers or cookies returns a
`Response`:

```go
eventhandler.Post(app, "/todos", func(_ context.Context, in CreateTodo) (eventhandler.Response[Todo], error) {
	return eventhandler.Response[Todo]{
		StatusCode: http.StatusCreated,
		Header:     http.Header{"Location": {"/todos/7"}},
		Cookies:    []*http.Cookie{{Name: "last", Value: "7", Secure: true, HttpOnly: true}},
		Body:       in.Body,
	}, nil
}, eventhandler.StatusCode(http.StatusCreated))
```

Plain handlers write what they want; `WriteJSON` writes a value as JSON with
the router's serializer.

### Compress

`Compress()` gzips the responses of a route when the request accepts gzip;
`Response.Compress` decides for one response. Compressed bodies are sent
base64-encoded.

### Binary responses

Bodies that are not text (binary content types, invalid UTF-8, compressed
bodies) are base64-encoded with `isBase64Encoded`. An API Gateway REST API
needs the content type among its binary media types; do not use `*/*`, which
breaks CORS pre-flight responses.

### Cache control

`CacheControl("max-age=3600")` sets `Cache-Control` on the 200 responses of
a route, and `no-cache` on the others.

### Debug mode

`WithDebug(true)` (or `POWERTOOLS_DEV=true`) answers an unhandled error with
a 500 `text/plain` body holding the error and the stack, instead of failing
the invocation, and writes what the router does to the Powertools debug log
(`POWERTOOLS_DEBUG=true`). Never in production: the stack is visible to
callers.

### Custom serializer

`WithSerializer` replaces the JSON encoder of response bodies (the default
is `encoding/json/v2`, compact, with sorted map keys), `WithDeserializer` the
decoder of typed operations' bodies.

### Split routes with Router

A router is also a group of routes: build one per area and include it with a
prefix.

```go
todos := eventhandler.New()
todos.HandleFunc("GET /", listTodos)
todos.HandleFunc("GET /{id}", getTodo)

app := eventhandler.New()
app.Include(todos, "/todos")
```

As in Python, the middlewares of the included router join the including
router's (they run for all its routes, after the middlewares it had when
`Include` was called), its error handlers replace those of the same type,
and a `/` route becomes the prefix itself. `Include` copies the routes the
router has at that moment.

### Local development

The router is an `http.Handler`: serve it locally, Python's
`HttpResolverLocal` without the ASGI server:

```go
func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") == "" {
		server := &http.Server{Addr: ":8080", Handler: app, ReadHeaderTimeout: 5 * time.Second}
		if err := server.ListenAndServe(); err != nil {
			slog.Error("server stopped", "err", err)
		}
		return
	}
	lambda.Start(eventhandler.APIGatewayREST(app))
}
```

Outside Lambda there is no raw event in the context and an unhandled error
answers 500.

### Considerations

A monolithic function (one router for the API) has one cold start and one
deployment; micro functions (one router per area, one function each) scale
and secure each area apart. Go cold starts are short either way. With micro
functions, each package exports the function that registers its routes, so
that a build step can merge their OpenAPI documents
(see [OpenAPI merge](eventhandler-openapi.md#openapi-merge)).

Typed operations pay their reflection at registration, during the cold start:
about 0.3 ms per operation once warm, 1 to 1.5 ms for the first ones (one
vCPU, that is 1,769 MB; proportionally more on smaller functions). In
exchange, a struct tag that does not make sense stops the function as it
starts, never on a request. Plain `http.Handler` routes cost nothing to
register. The package itself initializes in about 1 ms: its validator is
built in, and only `eventhandler/schemavalidation` links the full JSON Schema
library that package `validation` uses.

## Testing your code

Routers are `http.Handler`s: test them with `net/http/httptest`.

```go
func TestGetTodo(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/todos/1", nil)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}
```

To test the Lambda contract, call the adapter with an event, such as the
sample events of the Powertools test suite:

```go
handler := eventhandler.APIGatewayREST(app)
resp, err := handler(ctx, events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/todos/1"})
```

## Differences from Powertools Python

* Handlers are `net/http` handlers and typed operations; there are no
  resolver classes, no `app.current_event` (read the event from the request
  context), no `append_context` (context values) and no `Depends()`.
* Patterns use `{name}` and `{name...}` instead of `<name>`; path values are
  percent-decoded.
* Repeated query parameters of HTTP APIs and Function URLs stay separate;
  Python splits single values on commas.
* ALB query strings are always decoded (`decode_query_parameters`); ALB and
  VPC Lattice responses carry a status description; VPC Lattice responses
  send the last cookie as a `Set-Cookie` header, where Python puts them in a
  field Lattice ignores.
* Errors raised in handlers reach the middlewares around them as responses.
* Responses carry `Vary: Origin` when CORS allows specific origins, and
  `Vary: Accept-Encoding` on routes that compress, so that caches keep the
  variants apart.
* A body flagged base64 that is not is passed as it is, not rejected.
* `schemavalidation.Middleware` validates responses against the outbound
  schema; Python validates them against the inbound one, and only with
  outbound formats.
* Not ported: `resolve_async` (goroutines), `dependency_overrides`
  (constructor injection), the specialized routers (one `Router` serves every
  source).
