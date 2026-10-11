// Package schemavalidation validates the JSON bodies of the requests and
// responses of an eventhandler.Router against JSON Schemas, with the
// validation package: Powertools for AWS Lambda (Python)'s
// SchemaValidationMiddleware.
//
//	var orderSchema = validation.MustCompile(orderSchemaJSON)
//
//	app.Handle("POST /orders", createOrder,
//		eventhandler.Middlewares(schemavalidation.Middleware(orderSchema, nil)))
//
// A request whose body breaks the inbound schema answers 400 with
// {"statusCode":400,"message":"Bad Request: <message>"}; with an outbound
// schema, a response that breaks it answers 400 "Bad Response: <message>",
// as in Python; a schema that does not compile answers 500 "Internal Server
// Error". An empty body is validated as null.
//
// It lives apart from the eventhandler package so that functions that do
// not use it do not load a general JSON Schema engine, whose initialization
// costs milliseconds of every cold start. Typed operations validate their
// input without it.
//
// # Differences from Powertools Python
//
// Python validates the response only when outbound formats are given, and
// then against the inbound schema; this middleware validates it against the
// outbound schema whenever there is one. A body that is not JSON answers
// 400; Python fails with an unhandled error.
package schemavalidation
