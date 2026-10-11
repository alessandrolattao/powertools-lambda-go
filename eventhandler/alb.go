package eventhandler

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
)

// ALBMaxResponseSize is the largest response an Application Load Balancer
// accepts from a Lambda target, in bytes.
const ALBMaxResponseSize = 1 << 20

// ALBOption configures the ALB adapter.
type ALBOption func(*albConfig)

type albConfig struct {
	validateSize bool
}

// WithResponseSizeValidation checks the size of the serialized response
// against ALB's 1 MB limit, Python's enable_response_size_validation: a
// bigger response is a *ResponseSizeExceededError, which an error handler
// registered on the router (see OnError) can turn into a smaller response;
// that response is checked again, and the invocation fails when it is still
// too big or nobody handled the error.
func WithResponseSizeValidation() ALBOption {
	return func(c *albConfig) { c.validateSize = true }
}

// ALB adapts h to an Application Load Balancer target group: Python's
// ALBResolver.
//
// When the target group has multi-value headers on, the request carries
// every value of each header and query parameter, and so does the response;
// otherwise the response keeps the last value of each header and the last
// cookie, with a warning, as Python does. Query strings arrive
// percent-encoded from ALB and stay so in the URL, so the values read from
// it are decoded: Python's decode_query_parameters is always on.
func ALB(h http.Handler, opts ...ALBOption) func(context.Context, events.ALBTargetGroupRequest) (events.ALBTargetGroupResponse, error) {
	var cfg albConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return func(ctx context.Context, e events.ALBTargetGroupRequest) (events.ALBTargetGroupResponse, error) {
		multi := len(e.MultiValueHeaders) > 0
		header := singleHeaders(e.Headers)
		if multi {
			header = multiHeaders(e.MultiValueHeaders)
		}
		inv := &lambdahttp.Invocation{Event: &e}
		buf, req, err := serve(ctx, h, inv, requestParts{
			method: e.HTTPMethod, path: e.Path, rawQuery: albQuery(e), header: header,
			body: e.Body, base64: e.IsBase64Encoded, remoteAddr: forwardedFor(header),
		})
		if err != nil {
			return events.ALBTargetGroupResponse{}, err
		}
		resp := albResponse(buf, multi)
		if !cfg.validateSize {
			return resp, nil
		}
		sizeErr := checkALBSize(resp)
		if sizeErr == nil {
			return resp, nil
		}
		router, ok := h.(*Router)
		if !ok {
			return events.ALBTargetGroupResponse{}, sizeErr
		}
		buf, err = router.serveError(req, sizeErr)
		if err != nil {
			return events.ALBTargetGroupResponse{}, err
		}
		resp = albResponse(buf, multi)
		if err := checkALBSize(resp); err != nil {
			return events.ALBTargetGroupResponse{}, err
		}
		return resp, nil
	}
}

// albQuery rebuilds the query string from the values ALB passes as they
// were sent, still percent-encoded.
func albQuery(e events.ALBTargetGroupRequest) string {
	var parts []string
	if len(e.MultiValueQueryStringParameters) > 0 {
		for k, values := range e.MultiValueQueryStringParameters {
			for _, v := range values {
				parts = append(parts, k+"="+v)
			}
		}
	} else {
		for k, v := range e.QueryStringParameters {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "&")
}

func albResponse(buf *lambdahttp.ResponseBuffer, multi bool) events.ALBTargetGroupResponse {
	body, b64 := encodeBody(buf.Header(), buf.Body())
	status := buf.StatusCode()
	resp := events.ALBTargetGroupResponse{
		StatusCode:        status,
		StatusDescription: statusDescription(status),
		Body:              body,
		IsBase64Encoded:   b64,
	}
	if multi {
		resp.MultiValueHeaders = buf.Header()
		return resp
	}
	if cookies := buf.Header().Values("Set-Cookie"); len(cookies) > 1 {
		warnOnce("alb:Set-Cookie", func(l *slog.Logger) {
			l.Warn("Can only send one cookie without multiValueHeaders on the ALB Target Group; sending the last one",
				"cookies", len(cookies))
		})
	}
	resp.Headers = lastValueHeaders(buf.Header(), "alb")
	return resp
}

// checkALBSize serializes the response as aws-lambda-go does (HTML-safe
// escaping, nil maps as null) and compares its size with ALB's limit.
func checkALBSize(resp events.ALBTargetGroupResponse) error {
	data, err := json.Marshal(resp,
		jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true),
		json.FormatNilMapAsNull(true), json.FormatNilSliceAsNull(true))
	if err != nil {
		return fmt.Errorf("event handler: measure the response: %w", err)
	}
	if len(data) > ALBMaxResponseSize {
		return &ResponseSizeExceededError{ActualSize: len(data), MaxSize: ALBMaxResponseSize}
	}
	return nil
}

// serveError answers req with err through the error handlers and the
// route's response handling, without running the route or the middlewares
// again: how Python answers a response that turned out too big.
func (r *Router) serveError(req *http.Request, err error) (*lambdahttp.ResponseBuffer, error) {
	inv := lambdahttp.FromContext(req.Context())
	escaped := r.stripPrefixes(req.URL.EscapedPath())
	rt, _ := r.match(req.Method, escaped, req.URL.Path, inv != nil && inv.TrailingSlash)
	st := &requestState{router: r, route: rt}
	routed := req.WithContext(context.WithValue(req.Context(), stateKey{}, st))
	st.request = routed
	buf := lambdahttp.NewResponseBuffer()
	r.handleError(buf, routed, err)
	if inv != nil && inv.Err != nil {
		return nil, inv.Err
	}
	r.finish(buf, routed, st)
	return buf, nil
}
