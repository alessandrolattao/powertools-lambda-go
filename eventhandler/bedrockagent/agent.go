package bedrockagent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// MessageVersion is the version of the response format.
const MessageVersion = "1.0"

// DefaultTitle is the title of an agent's OpenAPI document when the router
// sets none, as in Powertools Python.
const DefaultTitle = "Powertools API"

// Errors of the OpenAPI export.
var (
	// ErrMissingDescription reports an operation without a description,
	// which Bedrock agents need to choose it.
	ErrMissingDescription = errors.New("bedrockagent: Bedrock agents require a description on every operation")
	// ErrSecurityUnsupported reports an operation with security
	// requirements, which agent action groups do not support.
	ErrSecurityUnsupported = errors.New("bedrockagent: Bedrock agents do not support security requirements on operations")
)

// Response is what the Lambda function of an OpenAPI action group returns to
// the agent.
type Response struct {
	MessageVersion              string                       `json:"messageVersion"`
	Response                    APIResponse                  `json:"response"`
	SessionAttributes           map[string]string            `json:"sessionAttributes,omitempty"`
	PromptSessionAttributes     map[string]string            `json:"promptSessionAttributes,omitempty"`
	KnowledgeBasesConfiguration []KnowledgeBaseConfiguration `json:"knowledgeBasesConfiguration,omitempty"`
}

// APIResponse is the result of the API operation the agent called.
type APIResponse struct {
	ActionGroup    string `json:"actionGroup"`
	APIPath        string `json:"apiPath"`
	HTTPMethod     string `json:"httpMethod"`
	HTTPStatusCode int    `json:"httpStatusCode"`
	// ResponseBody holds the body by content type.
	ResponseBody map[string]ResponseContent `json:"responseBody"`
}

// ResponseContent is a response body.
type ResponseContent struct {
	Body string `json:"body"`
}

// KnowledgeBaseConfiguration changes how the agent queries a knowledge base
// for the rest of the session.
type KnowledgeBaseConfiguration struct {
	KnowledgeBaseID        string         `json:"knowledgeBaseId"`
	RetrievalConfiguration map[string]any `json:"retrievalConfiguration,omitempty"`
}

// attributes are what a handler adds to the agent response, besides the
// body.
type attributes struct {
	mu             sync.Mutex
	session        map[string]string
	prompt         map[string]string
	knowledgeBases []KnowledgeBaseConfiguration
}

type attributesKey struct{}

func attributesFrom(ctx context.Context) *attributes {
	if a, ok := ctx.Value(attributesKey{}).(*attributes); ok {
		return a
	}
	return nil
}

// SetSessionAttributes sets the session attributes the response carries,
// Python's BedrockResponse session_attributes. ctx is the request's context
// (a typed operation's ctx, or r.Context()); outside an agent invocation the
// call does nothing.
func SetSessionAttributes(ctx context.Context, attrs map[string]string) {
	if a := attributesFrom(ctx); a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.session = attrs
	}
}

// SetPromptSessionAttributes sets the prompt session attributes the
// response carries.
func SetPromptSessionAttributes(ctx context.Context, attrs map[string]string) {
	if a := attributesFrom(ctx); a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.prompt = attrs
	}
}

// SetKnowledgeBasesConfiguration sets the knowledge base configurations the
// response carries.
func SetKnowledgeBasesConfiguration(ctx context.Context, cfg []KnowledgeBaseConfiguration) {
	if a := attributesFrom(ctx); a != nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.knowledgeBases = cfg
	}
}

// EventFromContext returns the agent event the request was built from:
// Python's current_event (session id, input text, agent...).
func EventFromContext(ctx context.Context) (eventsource.BedrockAgentEvent, bool) {
	if inv := lambdahttp.FromContext(ctx); inv != nil {
		if e, ok := inv.Event.(*eventsource.BedrockAgentEvent); ok {
			return *e, true
		}
	}
	return eventsource.BedrockAgentEvent{}, false
}

// Handler adapts h, usually an eventhandler.Router, to an agent action
// group defined with an OpenAPI schema: Python's BedrockAgentResolver.
//
// The request is the API call the agent decided on: its method and its API
// path, with the path parameters filled in from the parameters of the same
// name; every parameter is a query parameter too, never split on commas;
// the application/json body properties form a JSON object, their values
// converted to the JSON type the agent declares (integer, number, boolean,
// array). The response carries the status and the body under its content
// type, plus the attributes set with SetSessionAttributes,
// SetPromptSessionAttributes and SetKnowledgeBasesConfiguration.
func Handler(h http.Handler) func(context.Context, eventsource.BedrockAgentEvent) (Response, error) {
	return func(ctx context.Context, e eventsource.BedrockAgentEvent) (Response, error) {
		attrs := &attributes{}
		inv := &lambdahttp.Invocation{Event: &e}
		ctx = lambdahttp.NewContext(context.WithValue(ctx, attributesKey{}, attrs), inv)
		req, err := newRequest(ctx, e)
		if err != nil {
			return Response{}, err
		}
		buf, err := lambdahttp.Serve(h, req, inv)
		if err != nil {
			return Response{}, err
		}
		contentType := eventhandler.ContentTypeJSON
		if media, _, err := mime.ParseMediaType(buf.Header().Get("Content-Type")); err == nil && media != "" {
			contentType = media
		}
		attrs.mu.Lock()
		defer attrs.mu.Unlock()
		return Response{
			MessageVersion: MessageVersion,
			Response: APIResponse{
				ActionGroup:    e.ActionGroup,
				APIPath:        e.APIPath,
				HTTPMethod:     e.HTTPMethod,
				HTTPStatusCode: buf.StatusCode(),
				ResponseBody:   map[string]ResponseContent{contentType: {Body: string(buf.Body())}},
			},
			SessionAttributes:           attrs.session,
			PromptSessionAttributes:     attrs.prompt,
			KnowledgeBasesConfiguration: attrs.knowledgeBases,
		}, nil
	}
}

// newRequest builds the HTTP request of the agent's API call.
func newRequest(ctx context.Context, e eventsource.BedrockAgentEvent) (*http.Request, error) {
	query := url.Values{}
	path := e.APIPath
	for _, p := range e.Parameters {
		query.Add(p.Name, p.Value)
		// The API path is the operation's template; Powertools Python's
		// samples spell its parameters <name>.
		value := url.PathEscape(p.Value)
		path = strings.ReplaceAll(path, "{"+p.Name+"}", value)
		path = strings.ReplaceAll(path, "<"+p.Name+">", value)
	}
	var body []byte
	if e.RequestBody != nil {
		if media, ok := e.RequestBody.Content[eventhandler.ContentTypeJSON]; ok {
			obj := make(map[string]jsontext.Value, len(media.Properties))
			for _, p := range media.Properties {
				obj[p.Name] = typedValue(p)
			}
			var err error
			if body, err = json.Marshal(obj, json.Deterministic(true)); err != nil {
				return nil, fmt.Errorf("bedrockagent: encode the request body: %w", err)
			}
		}
	}
	u := &url.URL{Path: path, RawQuery: query.Encode()}
	if decoded, err := url.PathUnescape(path); err == nil {
		u.Path, u.RawPath = decoded, path
	}
	header := http.Header{}
	if body != nil {
		header.Set("Content-Type", eventhandler.ContentTypeJSON)
	}
	// The request is built by hand, as the other adapters build theirs: a
	// method the agent spells oddly reaches the router (and its 404), it
	// does not fail the invocation.
	req := &http.Request{
		Method:        strings.ToUpper(e.HTTPMethod),
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		RequestURI:    u.RequestURI(),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		},
	}
	return req.WithContext(ctx), nil
}

// typedValue converts a property the agent sends as a string to the JSON
// value of its declared type; a value that does not convert stays a string,
// for validation to report.
func typedValue(p eventsource.BedrockAgentProperty) jsontext.Value {
	asString := func() jsontext.Value {
		v, err := json.Marshal(p.Value)
		if err != nil {
			return jsontext.Value(`""`)
		}
		return v
	}
	switch p.Type {
	case "integer":
		if n, err := strconv.ParseInt(p.Value, 10, 64); err == nil {
			return jsontext.Value(strconv.FormatInt(n, 10))
		}
	case "number":
		if f, err := strconv.ParseFloat(p.Value, 64); err == nil {
			if v, err := json.Marshal(f); err == nil {
				return v
			}
		}
	case "boolean":
		switch strings.ToLower(p.Value) {
		case "true":
			return jsontext.Value("true")
		case "false":
			return jsontext.Value("false")
		}
	case "array", "object":
		if v := jsontext.Value(p.Value); v.IsValid() {
			return v
		}
	}
	return asString()
}

// OpenAPIJSON returns the router's OpenAPI document in version 3.0.3, the
// one Bedrock agents read, as the indented JSON to upload as the action
// group's API schema: Python's BedrockAgentResolver.get_openapi_json_schema.
// The title defaults to DefaultTitle. Every operation needs a description
// (ErrMissingDescription) and none may set security requirements
// (ErrSecurityUnsupported).
func OpenAPIJSON(r *eventhandler.Router, opts ...eventhandler.OpenAPIOption) ([]byte, error) {
	for _, route := range r.Routes() {
		if route.Description == "" {
			return nil, fmt.Errorf("%w: %s", ErrMissingDescription, route.Pattern)
		}
		if route.Security {
			return nil, fmt.Errorf("%w: %s", ErrSecurityUnsupported, route.Pattern)
		}
	}
	opts = append([]eventhandler.OpenAPIOption{func(c *eventhandler.OpenAPIConfig) {
		if c.Title == "" {
			c.Title = DefaultTitle
		}
	}}, opts...)
	return r.OpenAPI30JSON(opts...)
}

// RequireConfirmation asks the agent to confirm with the user before
// calling the operation: the x-requireConfirmation extension.
func RequireConfirmation() eventhandler.RouteOption {
	return eventhandler.Extension("x-requireConfirmation", "ENABLED")
}
