package eventsource

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// HTTPRequest is the set of events that carry an HTTP request: API Gateway
// REST, HTTP and WebSocket APIs, Application Load Balancer, Lambda Function
// URL and VPC Lattice. The generic helpers below (Body, Header, Cookies...)
// accept any of them, so that a function reads every HTTP source the same
// way, as Powertools Python's BaseProxyEvent does.
type HTTPRequest interface {
	events.APIGatewayProxyRequest |
		events.APIGatewayV2HTTPRequest |
		events.APIGatewayWebsocketProxyRequest |
		events.ALBTargetGroupRequest |
		events.LambdaFunctionURLRequest |
		APIGatewayProxyRequest |
		VPCLatticeEvent |
		VPCLatticeEventV2
}

// httpView is what the helpers need from an HTTP event, whatever its source.
type httpView struct {
	body         string
	base64       bool
	headers      map[string]string
	multiHeaders map[string][]string
	query        map[string]string
	multiQuery   map[string][]string
	cookies      []string
	path         string
	method       string
	// commaJoined marks the sources that fold repeated headers and query
	// parameters into one comma-separated value: HTTP API, Function URL and
	// VPC Lattice v1.
	commaJoined bool
	// onlyMulti marks VPC Lattice v2, whose maps hold every value of a
	// header or parameter, with no single-value map next to them.
	onlyMulti bool
}

func view[E HTTPRequest](e E) httpView {
	switch v := any(e).(type) {
	case events.APIGatewayProxyRequest:
		return restView(v)
	case APIGatewayProxyRequest:
		return restView(v.APIGatewayProxyRequest)
	case events.APIGatewayV2HTTPRequest:
		return httpView{
			body: v.Body, base64: v.IsBase64Encoded, headers: v.Headers, query: v.QueryStringParameters,
			cookies: v.Cookies, path: httpAPIPath(v.RawPath, v.RequestContext.Stage),
			method: v.RequestContext.HTTP.Method, commaJoined: true,
		}
	case events.LambdaFunctionURLRequest:
		return httpView{
			body: v.Body, base64: v.IsBase64Encoded, headers: v.Headers, query: v.QueryStringParameters,
			cookies: v.Cookies, path: v.RawPath, method: v.RequestContext.HTTP.Method, commaJoined: true,
		}
	case events.APIGatewayWebsocketProxyRequest:
		return httpView{
			body: v.Body, base64: v.IsBase64Encoded, headers: v.Headers, multiHeaders: v.MultiValueHeaders,
			query: v.QueryStringParameters, multiQuery: v.MultiValueQueryStringParameters,
			path: v.Path, method: v.HTTPMethod,
		}
	case events.ALBTargetGroupRequest:
		return httpView{
			body: v.Body, base64: v.IsBase64Encoded, headers: v.Headers, multiHeaders: v.MultiValueHeaders,
			query: v.QueryStringParameters, multiQuery: v.MultiValueQueryStringParameters,
			path: v.Path, method: v.HTTPMethod,
		}
	case VPCLatticeEvent:
		return httpView{
			body: v.Body, base64: v.IsBase64Encoded, headers: v.Headers, query: v.QueryStringParameters,
			path: v.RawPath, method: v.Method, commaJoined: true,
		}
	case VPCLatticeEventV2:
		return httpView{
			body: v.Body, base64: v.IsBase64Encoded, multiHeaders: multi(v.Headers), multiQuery: multi(v.QueryStringParameters),
			path: v.Path, method: v.Method, onlyMulti: true,
		}
	default:
		return httpView{}
	}
}

func restView(v events.APIGatewayProxyRequest) httpView {
	return httpView{
		body: v.Body, base64: v.IsBase64Encoded, headers: v.Headers, multiHeaders: v.MultiValueHeaders,
		query: v.QueryStringParameters, multiQuery: v.MultiValueQueryStringParameters,
		path: v.Path, method: v.HTTPMethod,
	}
}

func multi(m map[string]StringValues) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// httpAPIPath strips the stage from the raw path of an HTTP API request, as
// Powertools Python does, but only when the path does start with it.
func httpAPIPath(rawPath, stage string) string {
	if stage == "" || stage == "$default" {
		return rawPath
	}
	prefix := "/" + stage
	if rest, ok := strings.CutPrefix(rawPath, prefix); ok && (rest == "" || rest[0] == '/') {
		return rest
	}
	return rawPath
}

// Body returns the request body, base64-decoded when the event says it is
// encoded. A body that is not text (an upload) stays intact: it is returned
// as bytes, not forced through UTF-8.
func Body[E HTTPRequest](e E) ([]byte, error) {
	v := view(e)
	return DecodeBody(v.body, v.base64)
}

// DecodeBody decodes a request body that may be base64 encoded.
func DecodeBody(body string, isBase64Encoded bool) ([]byte, error) {
	if !isBase64Encoded || body == "" {
		return []byte(body), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("decode the base64 body: %w", err)
	}
	return decoded, nil
}

// JSONBody decodes the request body, base64-decoded first when needed, as
// JSON into a T. An empty body gives the zero T, as Powertools Python's
// json_body gives None. Decoding is lenient (unknown members are ignored);
// the parser package adds validation.
func JSONBody[T any, E HTTPRequest](e E) (T, error) {
	var out T
	body, err := Body(e)
	if err != nil || len(body) == 0 {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode the JSON body into %T: %w", out, err)
	}
	return out, nil
}

// Header returns the value of the named header, matched case-insensitively.
// Sources that only fill the multi-value map (an ALB target group with
// multi-value headers on) answer from it, with the last value, which is what
// API Gateway puts in the single-value map. VPC Lattice v2 headers with
// several values come back joined with commas.
func Header[E HTTPRequest](e E, name string) (string, bool) {
	v := view(e)
	if value, ok := LookupHeader(v.headers, name); ok {
		return value, true
	}
	return v.single(lookupMulti(v.multiHeaders, name))
}

// HeaderValues returns every value of the named header, matched
// case-insensitively: the multi-value map when the source has one, otherwise
// the single value. Like Powertools Python's resolved_headers_field, the
// sources that fold repeated headers into one value (HTTP API, Function URL,
// VPC Lattice v1) have it split on commas.
func HeaderValues[E HTTPRequest](e E, name string) []string {
	v := view(e)
	if values := lookupMulti(v.multiHeaders, name); len(values) > 0 {
		return values
	}
	value, ok := LookupHeader(v.headers, name)
	if !ok {
		return nil
	}
	if v.commaJoined {
		return strings.Split(value, ",")
	}
	return []string{value}
}

// QueryParameter returns the value of the named query string parameter. The
// name matches exactly, as query strings are case-sensitive. A parameter
// present only in the multi-value map answers with its last value; VPC
// Lattice v2 parameters with several values come back joined with commas.
func QueryParameter[E HTTPRequest](e E, name string) (string, bool) {
	v := view(e)
	if value, ok := v.query[name]; ok {
		return value, true
	}
	return v.single(v.multiQuery[name])
}

// single folds the values of a multi-value map into one: joined with commas
// for VPC Lattice v2, whose map is the only one; the last value otherwise,
// which is what API Gateway and ALB put in the single-value map.
func (v httpView) single(values []string) (string, bool) {
	switch {
	case len(values) == 0:
		return "", false
	case v.onlyMulti:
		return strings.Join(values, ","), true
	default:
		return values[len(values)-1], true
	}
}

// QueryParameterValues returns every value of the named query string
// parameter, the way Powertools Python resolves them for validation: the
// multi-value map wins when it has the parameter, otherwise the single value
// is split on commas.
func QueryParameterValues[E HTTPRequest](e E, name string) []string {
	v := view(e)
	if values := v.multiQuery[name]; len(values) > 0 {
		return values
	}
	value, ok := v.query[name]
	if !ok {
		return nil
	}
	return strings.Split(value, ",")
}

// Cookies returns the request cookies by name. HTTP API and Function URL
// events carry them in their cookies list; the other sources in the Cookie
// header (single or multi-value). Parsing is as lenient as Powertools
// Python's: segments without "=" are skipped, values are not unquoted, a
// later duplicate wins.
func Cookies[E HTTPRequest](e E) map[string]string {
	v := view(e)
	if v.cookies != nil {
		return ParseCookies(strings.Join(v.cookies, "; "))
	}
	if value, ok := LookupHeader(v.headers, "cookie"); ok && value != "" {
		return ParseCookies(value)
	}
	if values := lookupMulti(v.multiHeaders, "cookie"); len(values) > 0 {
		return ParseCookies(strings.Join(values, "; "))
	}
	return map[string]string{}
}

// Path returns the request path. For HTTP API events it is the raw path
// without the stage prefix (unless the stage is $default), as Powertools
// Python routes it.
func Path[E HTTPRequest](e E) string {
	return view(e).path
}

// Method returns the HTTP method of the request.
func Method[E HTTPRequest](e E) string {
	return view(e).method
}

// ParseCookies parses a Cookie header value into a map of cookie names to
// values. It follows Powertools Python's lenient parser rather than
// net/http's strict one: a malformed segment is skipped, not an error.
func ParseCookies(header string) map[string]string {
	cookies := map[string]string{}
	for segment := range strings.SplitSeq(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(segment), "=")
		if !ok {
			continue
		}
		cookies[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return cookies
}

// LookupHeader looks name up in a header map case-insensitively, the way
// Powertools Python's CaseInsensitiveDict does. An exact match wins over a
// case-folded one.
func LookupHeader(headers map[string]string, name string) (string, bool) {
	if value, ok := headers[name]; ok {
		return value, true
	}
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

func lookupMulti(headers map[string][]string, name string) []string {
	if values, ok := headers[name]; ok {
		return values
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}
