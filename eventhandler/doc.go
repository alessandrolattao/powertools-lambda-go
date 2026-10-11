// Package eventhandler routes the HTTP requests that reach a Lambda function
// through Amazon API Gateway (REST and HTTP APIs), an Application Load
// Balancer, a Lambda Function URL or VPC Lattice: the Go side of Powertools
// for AWS Lambda (Python)'s REST and HTTP event handler, with its data
// validation, OpenAPI document and Swagger UI.
//
// # net/http at the core
//
// A Router is an http.Handler. Routes are http.Handlers, middlewares are
// func(http.Handler) http.Handler, tests use net/http/httptest, and local
// development is http.ListenAndServe(":8080", app), Python's
// HttpResolverLocal for free. An adapter per event source turns the router
// into a Lambda handler: it builds the *http.Request from the event (method,
// path, query string with every value, headers, cookies, body decoded from
// base64, the caller's address) and the event's response from what the
// router wrote.
//
//	app := eventhandler.New()
//	app.HandleFunc("GET /todos", listTodos)
//	eventhandler.Get(app, "/todos/{id}", getTodo)
//	lambda.Start(eventhandler.APIGatewayREST(app))
//
//	Source                          Adapter          Python
//	API Gateway REST, HTTP API 1.0  APIGatewayREST   APIGatewayRestResolver
//	API Gateway HTTP API 2.0        APIGatewayHTTP   APIGatewayHttpResolver
//	Application Load Balancer       ALB              ALBResolver
//	Lambda Function URL (buffered)  FunctionURL      LambdaFunctionUrlResolver
//	VPC Lattice, version 1          VPCLattice       VPCLatticeResolver
//	VPC Lattice, version 2          VPCLatticeV2     VPCLatticeV2Resolver
//
// The raw event is in the request context: APIGatewayRESTEvent,
// APIGatewayHTTPEvent, ALBEvent, FunctionURLEvent, VPCLatticeEvent and
// VPCLatticeV2Event return it, lambdacontext.FromContext the Lambda context.
// Bedrock agents are served by the bedrockagent package, on the same router.
//
// # Routing
//
// Patterns use net/http's syntax ("GET /todos/{id}", "/files/{path...}")
// and Powertools' rules: a pattern matches the whole path; routes without
// wildcards are tried first, then the others in registration order; a method
// mismatch is a 404; GET does not answer HEAD; nothing redirects. Read path
// values with r.PathValue. HandleRegexp registers Python's regular
// expression routes. REST API routes ignore trailing slashes, as Python's
// REST resolver does. WithStripPrefixes and WithStripPrefixRegexps remove
// the base path of a custom domain mapping before routing. Registering the
// same route twice panics.
//
// # Typed operations
//
// Register (and Get, Post, Put, Patch, Delete, Head) registers a function
// func(ctx, In) (Out, error): In binds path, query, header and cookie
// parameters, form fields and files, and the JSON body (a field named Body)
// from struct tags; Out is the JSON response, or a Response choosing its
// status, headers and cookies. Constraints in the tags of
// github.com/swaggest/jsonschema-go (minimum, maxLength, pattern, enum,
// format, required...) generate one schema per operation that validates
// the request, describes the operation in the OpenAPI document and, with
// WithResponseValidation or ResponseValidation, validates the response. A
// request that does not conform answers 422 with Python's body:
//
//	{"statusCode":422,"detail":[{"loc":["query","limit"],"type":"int_parsing"}]}
//
// and a body of an unsupported content type 415. RequestFromContext gives a
// typed operation the request it serves.
//
// # Errors
//
// A typed operation fails by returning an error, a middleware or a plain
// handler by calling Error. A *ServiceError (BadRequest, Unauthorized,
// Forbidden, NotFound, RequestTimeout, RequestEntityTooLarge,
// InternalServerError, ServiceUnavailable) answers its status with
// {"statusCode":...,"message":...}. OnError registers handlers by error type
// (see its precedence rules), HandleNotFound the handler of unmatched
// requests. Any other error fails the invocation, as an exception does in
// Python, unless debug mode (WithDebug, POWERTOOLS_DEV) answers 500 with the
// error and the stack. Panics are not recovered.
//
// # Middlewares
//
// Use adds middlewares to every request, matched or not; Middlewares adds
// them to one route, after the router's. A middleware short-circuits by
// writing a response without calling next. Include merges the routes of
// another router under a prefix, with its middlewares and error handlers,
// as Python's include_router does. SchemaValidation validates JSON bodies
// against a JSON Schema with the validation package.
//
// # Responses
//
// WithCORS answers pre-flight requests and adds the CORS headers (route
// option CORS turns them off or on per route); Compress gzips responses for
// clients that accept it; CacheControl sets Cache-Control on 200 responses
// and no-cache on others. Bodies that are not text (binary content types,
// compressed bodies) are base64-encoded for the proxy response. Cookies set
// with http.SetCookie travel in multiValueHeaders (REST, ALB with
// multi-value headers), in the cookies list (HTTP API, Function URL) or as
// the last Set-Cookie header (ALB single-value, VPC Lattice). ALB checks the
// 1 MB response limit with WithResponseSizeValidation.
//
// # OpenAPI
//
// OpenAPI and OpenAPIJSON return the OpenAPI 3.1 document of the routes,
// built with github.com/swaggest/openapi-go; WithOpenAPI sets the title,
// version, servers, tags, security schemes and the rest, route options the
// summary, description, tags, operationId, security, extensions, responses.
// MergeOpenAPI merges the documents of several functions. The swaggerui
// package serves the Swagger UI.
//
// # Options
//
// Router: WithCORS, WithStripPrefixes, WithStripPrefixRegexps, WithDebug,
// WithSerializer, WithDeserializer, WithResponseValidation, WithOpenAPI.
// Route: CORS, Compress, CacheControl, StatusCode, Middlewares,
// ResponseValidation, Summary, Description, Tags, OperationID, Deprecated,
// ExcludeFromSchema, Security, Extension, ResponseDescription,
// ResponseModel.
//
// # Environment variables
//
// POWERTOOLS_DEV turns debug mode on unless WithDebug says otherwise.
// POWERTOOLS_DEBUG shows what the router does in the Powertools debug log.
//
// # Differences from Powertools Python
//
//   - Handlers are net/http handlers and typed operations; there are no
//     resolver classes per source, no current_event on the app (read it from
//     the request context), no append_context (use context values) and no
//     Depends: middlewares put values in the request context and handlers
//     read them with typed getters.
//   - Patterns use net/http's {name} and {name...} instead of <name>; path
//     values are percent-decoded, Python passes them encoded.
//   - Parameters are bound by struct tags instead of signature annotations;
//     JSON bodies decode strictly ("1" is not an integer), Python coerces.
//     Required parameters say so with required:"true"; Go has no "no
//     default".
//   - Repeated query parameters of HTTP APIs and Function URLs stay
//     separate (the adapter reads the raw query string); Python splits
//     single values on commas. Header list parameters split on commas for
//     every source.
//   - A typed operation's string output is a JSON string; Python sends str
//     bodies unquoted. Use a []byte body for raw content.
//   - Response validation failures are ResponseValidationErrors with the
//     configured status, 422 by default; Python reports them as request
//     validation errors when no status is configured.
//   - Errors from middlewares and handlers are answered where they happen,
//     so the middlewares around them see the error response; in Python the
//     exception skips their "after" logic.
//   - Router middlewares and error handlers merge on Include as in Python;
//     a router included elsewhere keeps no options of its own.
//   - ALB query strings arrive percent-encoded and are decoded by url.Values:
//     decode_query_parameters is always on. ALB and VPC Lattice responses
//     carry a status description. VPC Lattice responses send the last cookie
//     as a Set-Cookie header (Python puts cookies in a field Lattice
//     ignores).
//   - Responses carry Vary: Origin when CORS allows specific origins, and
//     Vary: Accept-Encoding on routes that compress, so that caches keep the
//     variants apart.
//   - The OpenAPI validation error schemas list statusCode and do not
//     require msg, matching the bodies actually sent; components are named
//     after the Go type; regular expression routes and routes without a
//     method are not documented; bodies of GET, HEAD and DELETE operations
//     are bound and validated but not documented.
//   - SchemaValidation validates responses against the outbound schema;
//     Python validates them against the inbound one, and only when outbound
//     formats are set.
//   - A multipart body without a boundary answers 422 (multipart_invalid);
//     Python raises an unhandled error.
//   - Not ported: dependency_overrides (inject dependencies with closures or
//     interfaces), resolve_async (handlers use goroutines), the specialized
//     routers (one Router type serves every source), the custom JSON body
//     deserializer for non-typed routes (handlers read r.Body).
package eventhandler
