package eventhandler

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// requestParts is what an adapter extracts from its event to build the
// *http.Request.
type requestParts struct {
	method     string
	path       string
	rawQuery   string
	header     http.Header
	body       string
	base64     bool
	remoteAddr string
	host       string
}

// newRequest builds the request the handler serves.
func newRequest(ctx context.Context, p requestParts) *http.Request {
	body, err := eventsource.DecodeBody(p.body, p.base64)
	if err != nil {
		// Python decodes the body only when a handler reads it; a body
		// flagged base64 that is not must not stop the routing.
		body = []byte(p.body)
	}
	path := p.path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	u := &url.URL{Path: path, RawQuery: p.rawQuery}
	// Event paths arrive percent-encoded when the client encoded them; keep
	// the encoding so that "%2F" inside a segment stays a single segment.
	if decoded, err := url.PathUnescape(path); err == nil {
		u.Path, u.RawPath = decoded, path
	}
	if p.header == nil {
		p.header = http.Header{}
	}
	host := p.host
	if h := p.header.Get("Host"); h != "" {
		host = h
	}
	req := &http.Request{
		Method:        strings.ToUpper(p.method),
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        p.header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Host:          host,
		RemoteAddr:    p.remoteAddr,
		RequestURI:    u.RequestURI(),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		},
	}
	return req.WithContext(ctx)
}

// serve builds the request from p and runs h on it.
func serve(ctx context.Context, h http.Handler, inv *lambdahttp.Invocation, p requestParts) (*lambdahttp.ResponseBuffer, *http.Request, error) {
	req := newRequest(lambdahttp.NewContext(ctx, inv), p)
	buf, err := lambdahttp.Serve(h, req, inv)
	return buf, req, err
}

func singleHeaders(headers map[string]string) http.Header {
	h := make(http.Header, len(headers))
	for k, v := range headers {
		h.Add(k, v)
	}
	return h
}

func multiHeaders(headers map[string][]string) http.Header {
	h := make(http.Header, len(headers))
	for k, values := range headers {
		for _, v := range values {
			h.Add(k, v)
		}
	}
	return h
}

// encodeQuery encodes decoded query parameters, preferring the multi-value
// map, as API Gateway fills both.
func encodeQuery(single map[string]string, multi map[string][]string) string {
	if len(multi) > 0 {
		return url.Values(multi).Encode()
	}
	values := url.Values{}
	for k, v := range single {
		values.Set(k, v)
	}
	return values.Encode()
}

// forwardedFor returns the client address of a request forwarded by a load
// balancer.
func forwardedFor(h http.Header) string {
	first, _, _ := strings.Cut(h.Get("X-Forwarded-For"), ",")
	return strings.TrimSpace(first)
}

// encodeBody returns the body for the proxy response: as text when it is
// text, base64-encoded otherwise (binary content types, invalid UTF-8,
// compressed bodies), as Python base64-encodes bytes bodies.
func encodeBody(h http.Header, body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	if h.Get("Content-Encoding") == "" && isText(h.Get("Content-Type")) && utf8.Valid(body) {
		return string(body), false
	}
	return base64.StdEncoding.EncodeToString(body), true
}

func isText(contentType string) bool {
	if contentType == "" {
		return true
	}
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	if strings.HasPrefix(media, "text/") || strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml") {
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/javascript", "application/x-javascript",
		"application/ecmascript", "application/x-www-form-urlencoded", "application/graphql",
		"application/x-yaml", "application/yaml", "application/x-ndjson":
		return true
	default:
		return false
	}
}

// joinedHeaders folds every header but Set-Cookie into one value, the way
// HTTP API, Function URL and VPC Lattice responses carry them, and returns
// the cookies apart.
func joinedHeaders(h http.Header) (headers map[string]string, cookies []string) {
	headers = make(map[string]string, len(h))
	for _, name := range slices.Sorted(maps.Keys(h)) {
		if name == "Set-Cookie" {
			cookies = h[name]
			continue
		}
		headers[name] = strings.Join(h[name], ", ")
	}
	return headers, cookies
}

// lastValueHeaders keeps one value per header, the last, for responses that
// cannot carry more; Python warns about each header that loses values.
func lastValueHeaders(h http.Header, source string) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		if len(values) == 0 {
			continue
		}
		if len(values) > 1 {
			warnOnce(source+":"+name, func(l *slog.Logger) {
				l.Warn("Can only send one value per header; sending the last one. Did you enable multiValueHeaders on the ALB Target Group?",
					"source", source, "header", name)
			})
		}
		out[name] = values[len(values)-1]
	}
	return out
}

// APIGatewayREST adapts h to an API Gateway REST API proxy integration (and
// to an HTTP API with payload format 1.0): Python's APIGatewayRestResolver.
//
// The request carries the event's method, path, query string (every value
// of the multi-value map), headers, cookies, body (base64-decoded) and the
// caller's source IP as RemoteAddr; routes ignore trailing slashes, as in
// Python. The response puts every header, Set-Cookie included, in
// multiValueHeaders, and base64-encodes bodies that are not text, which
// needs binary media types configured on the API.
func APIGatewayREST(h http.Handler) func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	return func(ctx context.Context, e events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		header := multiHeaders(e.MultiValueHeaders)
		if len(e.MultiValueHeaders) == 0 {
			header = singleHeaders(e.Headers)
		}
		inv := &lambdahttp.Invocation{Event: &e, TrailingSlash: true, BasePath: stageBasePath(e.RequestContext.Stage, e.RequestContext.Path)}
		buf, _, err := serve(ctx, h, inv, requestParts{
			method: e.HTTPMethod, path: e.Path, rawQuery: encodeQuery(e.QueryStringParameters, e.MultiValueQueryStringParameters),
			header: header, body: e.Body, base64: e.IsBase64Encoded,
			remoteAddr: e.RequestContext.Identity.SourceIP, host: e.RequestContext.DomainName,
		})
		if err != nil {
			return events.APIGatewayProxyResponse{}, err
		}
		body, b64 := encodeBody(buf.Header(), buf.Body())
		return events.APIGatewayProxyResponse{
			StatusCode:        buf.StatusCode(),
			MultiValueHeaders: buf.Header(),
			Body:              body,
			IsBase64Encoded:   b64,
		}, nil
	}
}

// stageBasePath returns "/{stage}" when the request path starts with the
// stage, as it does on a stage URL but not on a custom domain or SAM local.
func stageBasePath(stage, path string) string {
	if stage == "" || stage == "$default" {
		return ""
	}
	if path == "/"+stage || strings.HasPrefix(path, "/"+stage+"/") {
		return "/" + stage
	}
	return ""
}

// APIGatewayHTTP adapts h to an API Gateway HTTP API with payload format
// 2.0: Python's APIGatewayHttpResolver.
//
// The path is the raw path without the stage prefix (unless the stage is
// $default), the query is the raw query string, so repeated parameters stay
// separate, and the cookies list becomes the Cookie header. The response
// joins header values with ", " and moves Set-Cookie headers to cookies.
func APIGatewayHTTP(h http.Handler) func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	return func(ctx context.Context, e events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		header := singleHeaders(e.Headers)
		if len(e.Cookies) > 0 {
			header.Set("Cookie", strings.Join(e.Cookies, "; "))
		}
		inv := &lambdahttp.Invocation{Event: &e, BasePath: stageBasePath(e.RequestContext.Stage, e.RequestContext.HTTP.Path)}
		buf, _, err := serve(ctx, h, inv, requestParts{
			method: e.RequestContext.HTTP.Method, path: eventsource.Path(e), rawQuery: e.RawQueryString,
			header: header, body: e.Body, base64: e.IsBase64Encoded,
			remoteAddr: e.RequestContext.HTTP.SourceIP, host: e.RequestContext.DomainName,
		})
		if err != nil {
			return events.APIGatewayV2HTTPResponse{}, err
		}
		headers, cookies := joinedHeaders(buf.Header())
		body, b64 := encodeBody(buf.Header(), buf.Body())
		return events.APIGatewayV2HTTPResponse{
			StatusCode:      buf.StatusCode(),
			Headers:         headers,
			Cookies:         cookies,
			Body:            body,
			IsBase64Encoded: b64,
		}, nil
	}
}

// FunctionURL adapts h to a Lambda Function URL in BUFFERED invoke mode:
// Python's LambdaFunctionUrlResolver. Requests and responses are those of
// APIGatewayHTTP. For RESPONSE_STREAM mode, serve the router with
// aws-lambda-go's lambdaurl.Start instead, which streams what it writes.
func FunctionURL(h http.Handler) func(context.Context, events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	return func(ctx context.Context, e events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		header := singleHeaders(e.Headers)
		if len(e.Cookies) > 0 {
			header.Set("Cookie", strings.Join(e.Cookies, "; "))
		}
		inv := &lambdahttp.Invocation{Event: &e}
		buf, _, err := serve(ctx, h, inv, requestParts{
			method: e.RequestContext.HTTP.Method, path: e.RawPath, rawQuery: e.RawQueryString,
			header: header, body: e.Body, base64: e.IsBase64Encoded,
			remoteAddr: e.RequestContext.HTTP.SourceIP, host: e.RequestContext.DomainName,
		})
		if err != nil {
			return events.LambdaFunctionURLResponse{}, err
		}
		headers, cookies := joinedHeaders(buf.Header())
		body, b64 := encodeBody(buf.Header(), buf.Body())
		return events.LambdaFunctionURLResponse{
			StatusCode:      buf.StatusCode(),
			Headers:         headers,
			Cookies:         cookies,
			Body:            body,
			IsBase64Encoded: b64,
		}, nil
	}
}

// VPCLatticeResponse is the response of a function behind a VPC Lattice
// target group, for both event structure versions.
type VPCLatticeResponse struct {
	StatusCode        int               `json:"statusCode"`
	StatusDescription string            `json:"statusDescription,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	Body              string            `json:"body"`
	IsBase64Encoded   bool              `json:"isBase64Encoded"`
}

// latticeResponse builds the VPC Lattice response. Lattice reads one value
// per header and has no cookies field (where Python puts the cookies, which
// Lattice drops): the last Set-Cookie goes out as a header, with a warning
// when there were more.
func latticeResponse(buf *lambdahttp.ResponseBuffer) VPCLatticeResponse {
	headers, cookies := joinedHeaders(buf.Header())
	if len(cookies) > 0 {
		if len(cookies) > 1 {
			warnOnce("vpc-lattice:Set-Cookie", func(l *slog.Logger) {
				l.Warn("VPC Lattice responses carry one cookie; sending the last one", "cookies", len(cookies))
			})
		}
		headers["Set-Cookie"] = cookies[len(cookies)-1]
	}
	body, b64 := encodeBody(buf.Header(), buf.Body())
	status := buf.StatusCode()
	return VPCLatticeResponse{
		StatusCode:        status,
		StatusDescription: statusDescription(status),
		Headers:           headers,
		Body:              body,
		IsBase64Encoded:   b64,
	}
}

func statusDescription(status int) string {
	return strings.TrimSpace(fmt.Sprintf("%d %s", status, http.StatusText(status)))
}

// VPCLattice adapts h to a VPC Lattice target group with event structure
// version 1: Python's VPCLatticeResolver. Repeated query parameters arrive
// folded with commas: list parameters of typed operations split them.
func VPCLattice(h http.Handler) func(context.Context, eventsource.VPCLatticeEvent) (VPCLatticeResponse, error) {
	return func(ctx context.Context, e eventsource.VPCLatticeEvent) (VPCLatticeResponse, error) {
		header := singleHeaders(e.Headers)
		path, rawQuery, _ := strings.Cut(e.RawPath, "?")
		if len(e.QueryStringParameters) > 0 || rawQuery == "" {
			rawQuery = encodeQuery(e.QueryStringParameters, nil)
		}
		inv := &lambdahttp.Invocation{Event: &e, CommaQuery: true}
		buf, _, err := serve(ctx, h, inv, requestParts{
			method: e.Method, path: path, rawQuery: rawQuery, header: header,
			body: e.Body, base64: e.IsBase64Encoded, remoteAddr: forwardedFor(header),
		})
		if err != nil {
			return VPCLatticeResponse{}, err
		}
		return latticeResponse(buf), nil
	}
}

// VPCLatticeV2 adapts h to a VPC Lattice target group with event structure
// version 2, the one AWS recommends: Python's VPCLatticeV2Resolver. Headers
// and query parameters keep all their values.
func VPCLatticeV2(h http.Handler) func(context.Context, eventsource.VPCLatticeEventV2) (VPCLatticeResponse, error) {
	return func(ctx context.Context, e eventsource.VPCLatticeEventV2) (VPCLatticeResponse, error) {
		header := http.Header{}
		for k, values := range e.Headers {
			for _, v := range values {
				header.Add(k, v)
			}
		}
		query := url.Values{}
		for k, values := range e.QueryStringParameters {
			query[k] = values
		}
		inv := &lambdahttp.Invocation{Event: &e}
		buf, _, err := serve(ctx, h, inv, requestParts{
			method: e.Method, path: e.Path, rawQuery: query.Encode(), header: header,
			body: e.Body, base64: e.IsBase64Encoded, remoteAddr: forwardedFor(header),
		})
		if err != nil {
			return VPCLatticeResponse{}, err
		}
		return latticeResponse(buf), nil
	}
}
