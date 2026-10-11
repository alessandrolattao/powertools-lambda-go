package eventhandler

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"regexp"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// Option configures a Router made with New. Options matter only on the
// router that serves requests: a router included in another one brings its
// routes, middlewares and error handlers, not its options.
type Option func(*config)

type config struct {
	cors               *CORSConfig
	prefixes           []stripPrefix
	debug              *bool
	serializer         func(v any) ([]byte, error)
	deserializer       func(data []byte, v any) error
	responseValidation int
	openapi            OpenAPIConfig
}

type stripPrefix struct {
	text string
	re   *regexp.Regexp
}

func defaultConfig() config {
	return config{
		serializer:   marshalJSON,
		deserializer: unmarshalJSON,
	}
}

// WithCORS enables CORS on every route, unless a route opts out with
// CORS(false), and answers pre-flight requests: Python's cors parameter.
func WithCORS(cfg CORSConfig) Option {
	return func(c *config) { c.cors = &cfg }
}

// WithStripPrefixes removes the first of the prefixes the request path
// starts with before routing, as Python's strip_prefixes does with strings:
// a path equal to a prefix becomes "/", "/payment/todos" becomes "/todos"
// for the prefix "/payment". It serves APIs mapped under a base path of a
// custom domain. Prefixes apply in the order the options give them.
func WithStripPrefixes(prefixes ...string) Option {
	return func(c *config) {
		for _, p := range prefixes {
			c.prefixes = append(c.prefixes, stripPrefix{text: p})
		}
	}
}

// WithStripPrefixRegexps removes every match of each expression from the
// request path before routing, as Python's strip_prefixes does with
// compiled patterns ("/v[1-3]+/(dev|stg)"): unlike a string prefix, a
// regular expression does not stop the stripping.
func WithStripPrefixRegexps(prefixes ...*regexp.Regexp) Option {
	return func(c *config) {
		for _, p := range prefixes {
			c.prefixes = append(c.prefixes, stripPrefix{re: p})
		}
	}
}

// WithDebug turns the debug mode on or off. It defaults to POWERTOOLS_DEV.
// In debug mode an unhandled error answers 500 with the error and the stack
// as text instead of failing the invocation, and the router writes what it
// does to the Powertools debug log (shown with POWERTOOLS_DEBUG).
func WithDebug(enabled bool) Option {
	return func(c *config) { c.debug = &enabled }
}

// WithSerializer replaces the JSON encoder of response bodies: Python's
// serializer. The default is encoding/json/v2's Marshal, compact, with map
// keys sorted as encoding/json sorts them.
func WithSerializer(marshal func(v any) ([]byte, error)) Option {
	return func(c *config) { c.serializer = marshal }
}

// WithDeserializer replaces the JSON decoder of the request bodies of typed
// operations: Python's json_body_deserializer. The default is
// encoding/json/v2's Unmarshal, accepting duplicate names as Python does.
func WithDeserializer(unmarshal func(data []byte, v any) error) Option {
	return func(c *config) { c.deserializer = unmarshal }
}

// WithResponseValidation validates what every typed operation returns
// against its response schema, and answers with status when it does not
// conform: Python's response_validation_error_http_code. A route can
// choose its own status with ResponseValidation.
func WithResponseValidation(status int) Option {
	return func(c *config) { c.responseValidation = status }
}

// WithOpenAPI sets the document-level fields of the OpenAPI document:
// Python's configure_openapi.
func WithOpenAPI(cfg OpenAPIConfig) Option {
	return func(c *config) { c.openapi = cfg }
}

// unmarshalJSON is the default deserializer. It accepts duplicate names,
// the last one winning, as Python's json module does.
func unmarshalJSON(data []byte, v any) error {
	if err := json.Unmarshal(data, v, jsontext.AllowDuplicateNames(true)); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// marshalJSON is the default serializer: deterministic, so that a map
// encodes the same way every time.
func marshalJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	return data, nil
}

func (c *config) debugMode() bool {
	if c.debug != nil {
		return *c.debug
	}
	return env.IsDev()
}

// RouteOption configures one route.
type RouteOption func(*routeConfig)

type routeConfig struct {
	cors                *bool
	compress            bool
	cacheControl        string
	summary             string
	description         string
	tags                []string
	operationID         string
	deprecated          bool
	hidden              bool
	security            []SecurityRequirement
	extensions          map[string]any
	status              int
	responseDescription string
	responses           []responseDoc
	middlewares         []Middleware
	responseValidation  int
}

type responseDoc struct {
	status      int
	description string
	model       any
}

func newRouteConfig(opts []RouteOption) routeConfig {
	cfg := routeConfig{status: http.StatusOK}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// CORS turns CORS on or off for the route, whatever the router does. A
// route that turns it on in a router without WithCORS uses CORSConfig's
// defaults.
func CORS(enabled bool) RouteOption {
	return func(c *routeConfig) { c.cors = &enabled }
}

// Compress gzips the response when the request accepts gzip.
func Compress() RouteOption {
	return func(c *routeConfig) { c.compress = true }
}

// CacheControl sets the Cache-Control header of 200 responses to value;
// other statuses get "no-cache".
func CacheControl(value string) RouteOption {
	return func(c *routeConfig) { c.cacheControl = value }
}

// Summary sets the summary of the operation in the OpenAPI document. It
// defaults to the method and the path.
func Summary(summary string) RouteOption {
	return func(c *routeConfig) { c.summary = summary }
}

// Description sets the description of the operation in the OpenAPI
// document; Bedrock agents require one.
func Description(description string) RouteOption {
	return func(c *routeConfig) { c.description = description }
}

// Tags groups the operation under tags in the OpenAPI document.
func Tags(tags ...string) RouteOption {
	return func(c *routeConfig) { c.tags = append(c.tags, tags...) }
}

// OperationID sets the operationId. It defaults to the handler's function
// name, the path and the method, as Python generates it.
func OperationID(id string) RouteOption {
	return func(c *routeConfig) { c.operationID = id }
}

// Deprecated marks the operation as deprecated in the OpenAPI document.
func Deprecated() RouteOption {
	return func(c *routeConfig) { c.deprecated = true }
}

// ExcludeFromSchema leaves the route out of the OpenAPI document: Python's
// include_in_schema=False.
func ExcludeFromSchema() RouteOption {
	return func(c *routeConfig) { c.hidden = true }
}

// Security sets the security requirements of the operation, by the names of
// the security schemes of the OpenAPI configuration. An empty requirement
// makes security optional. Powertools documents security; it does not
// enforce it.
func Security(requirements ...SecurityRequirement) RouteOption {
	return func(c *routeConfig) { c.security = append(c.security, requirements...) }
}

// Extension adds an OpenAPI extension to the operation; name must start with
// "x-".
func Extension(name string, value any) RouteOption {
	return func(c *routeConfig) {
		if c.extensions == nil {
			c.extensions = map[string]any{}
		}
		c.extensions[name] = value
	}
}

// StatusCode sets the status of the successful responses of a typed
// operation, and the status the OpenAPI document gives them. It defaults to
// 200.
func StatusCode(status int) RouteOption {
	return func(c *routeConfig) { c.status = status }
}

// ResponseDescription sets the description of the successful response in
// the OpenAPI document. It defaults to "Successful Response".
func ResponseDescription(description string) RouteOption {
	return func(c *routeConfig) { c.responseDescription = description }
}

// ResponseModel documents a response of the operation: its status, its
// description and the Go value its JSON body looks like (a nil model means
// the operation's own response type). As in Python, routes that document
// their responses this way replace the default successful response.
func ResponseModel(status int, description string, model any) RouteOption {
	return func(c *routeConfig) {
		c.responses = append(c.responses, responseDoc{status: status, description: description, model: model})
	}
}

// Middlewares adds middlewares to the route. They run after the router's
// own, in order.
func Middlewares(middlewares ...Middleware) RouteOption {
	return func(c *routeConfig) { c.middlewares = append(c.middlewares, middlewares...) }
}

// ResponseValidation validates the responses of the typed operation and
// answers status when one does not conform, whatever WithResponseValidation
// says: Python's custom_response_validation_http_code.
func ResponseValidation(status int) RouteOption {
	return func(c *routeConfig) { c.responseValidation = status }
}
