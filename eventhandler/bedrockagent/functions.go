package bedrockagent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Errors of function action groups.
var (
	// ErrUnknownFunction reports an event for a function no tool handles.
	ErrUnknownFunction = errors.New("unknown function")
	// ErrDuplicateTool reports a tool registered twice.
	ErrDuplicateTool = errors.New("bedrockagent: tool already registered")
	// ErrInvalidResponseState reports a ResponseState other than FAILURE
	// and REPROMPT.
	ErrInvalidResponseState = errors.New("responseState must be 'FAILURE' or 'REPROMPT'")
)

// ResponseState tells the agent how a function call went.
type ResponseState string

// The response states a function can report; the empty state is success.
const (
	Failure  ResponseState = "FAILURE"
	Reprompt ResponseState = "REPROMPT"
)

// FunctionResponse is what the Lambda function of a function action group
// returns to the agent.
type FunctionResponse struct {
	MessageVersion              string                       `json:"messageVersion"`
	Response                    FunctionResult               `json:"response"`
	SessionAttributes           map[string]string            `json:"sessionAttributes"`
	PromptSessionAttributes     map[string]string            `json:"promptSessionAttributes"`
	KnowledgeBasesConfiguration []KnowledgeBaseConfiguration `json:"knowledgeBasesConfiguration,omitempty"`
}

// FunctionResult is the result of the function the agent called.
type FunctionResult struct {
	ActionGroup      string               `json:"actionGroup"`
	Function         string               `json:"function"`
	FunctionResponse FunctionResponseBody `json:"functionResponse"`
}

// FunctionResponseBody holds the body, as TEXT, and the response state.
type FunctionResponseBody struct {
	ResponseBody  map[string]ResponseContent `json:"responseBody"`
	ResponseState ResponseState              `json:"responseState,omitempty"`
}

// Result is what a tool returns when it sets more than the body: Python's
// BedrockFunctionResponse.
type Result[T any] struct {
	Body                    T
	SessionAttributes       map[string]string
	PromptSessionAttributes map[string]string
	KnowledgeBases          []KnowledgeBaseConfiguration
	// ResponseState is empty, Failure or Reprompt.
	ResponseState ResponseState
}

type result interface {
	parts() Result[any]
}

func (r Result[T]) parts() Result[any] {
	return Result[any]{
		Body: r.Body, SessionAttributes: r.SessionAttributes, PromptSessionAttributes: r.PromptSessionAttributes,
		KnowledgeBases: r.KnowledgeBases, ResponseState: r.ResponseState,
	}
}

// Functions dispatches the calls of a function action group to tools:
// Python's BedrockAgentFunctionResolver.
type Functions struct {
	tools      map[string]*tool
	serializer func(v any) ([]byte, error)
}

type tool struct {
	description string
	call        func(ctx context.Context, params []byte) (any, error)
}

// FunctionsOption configures Functions.
type FunctionsOption func(*Functions)

// WithSerializer replaces the JSON encoder of the bodies that are not
// strings: Python's serializer. The default is encoding/json/v2's Marshal
// with map keys sorted.
func WithSerializer(marshal func(v any) ([]byte, error)) FunctionsOption {
	return func(f *Functions) { f.serializer = marshal }
}

// NewFunctions returns Functions without tools.
func NewFunctions(opts ...FunctionsOption) *Functions {
	f := &Functions{tools: map[string]*tool{}, serializer: func(v any) ([]byte, error) { return json.Marshal(v, json.Deterministic(true)) }}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// ToolOption configures a tool.
type ToolOption func(*tool)

// Description describes the tool. Agents read function descriptions from
// the action group's definition, not from the function: it documents the
// code, as in Python.
func Description(description string) ToolOption {
	return func(t *tool) { t.description = description }
}

// Tool registers fn as the function name of the action group: Python's
// tool decorator. The parameters the agent passes are converted to the JSON
// type they declare (string, integer, number, boolean, array) and decoded
// into In by name, with its json tags: unknown parameters are ignored and
// missing ones stay zero. What fn returns is the body sent to the agent: a
// string as it is, anything else as JSON; return a Result to set the
// session attributes or the response state too. Registering a name twice
// panics with ErrDuplicateTool.
func Tool[In, Out any](f *Functions, name string, fn func(ctx context.Context, in In) (Out, error), opts ...ToolOption) {
	if _, ok := f.tools[name]; ok {
		panic(fmt.Errorf("%w: %q", ErrDuplicateTool, name))
	}
	t := &tool{call: func(ctx context.Context, params []byte) (any, error) {
		var in In
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, fmt.Errorf("decode the parameters: %w", err)
		}
		return fn(ctx, in)
	}}
	for _, opt := range opts {
		opt(t)
	}
	f.tools[name] = t
}

type functionEventKey struct{}

// FunctionEventFromContext returns the event of the function call a tool is
// serving.
func FunctionEventFromContext(ctx context.Context) (eventsource.BedrockAgentFunctionEvent, bool) {
	e, ok := ctx.Value(functionEventKey{}).(*eventsource.BedrockAgentFunctionEvent)
	if !ok {
		return eventsource.BedrockAgentFunctionEvent{}, false
	}
	return *e, true
}

// Handle calls the tool of the event's function and returns its response:
// pass it to lambda.Start. Failures never fail the invocation, as in
// Python: an unknown function or an error of the tool answers the body
// "Error: <type>: <message>", the type being the Go type of the error.
// Session attributes default to the event's.
func (f *Functions) Handle(ctx context.Context, e eventsource.BedrockAgentFunctionEvent) (FunctionResponse, error) {
	ctx = context.WithValue(ctx, functionEventKey{}, &e)
	out, err := f.result(ctx, e)
	if err != nil {
		debug.Logger().DebugContext(ctx, "bedrockagent: function failed", "function", e.Function, "err", err)
		out = Result[any]{Body: "Error: " + typeName(err) + ": " + err.Error()}
	}
	text, err := f.text(out.Body)
	if err != nil {
		text = "Error: " + typeName(err) + ": " + err.Error()
	}
	return FunctionResponse{
		MessageVersion: MessageVersion,
		Response: FunctionResult{
			ActionGroup: e.ActionGroup,
			Function:    e.Function,
			FunctionResponse: FunctionResponseBody{
				ResponseBody:  map[string]ResponseContent{"TEXT": {Body: text}},
				ResponseState: out.ResponseState,
			},
		},
		SessionAttributes:           firstNonEmpty(out.SessionAttributes, e.SessionAttributes),
		PromptSessionAttributes:     firstNonEmpty(out.PromptSessionAttributes, e.PromptSessionAttributes),
		KnowledgeBasesConfiguration: out.KnowledgeBases,
	}, nil
}

// result calls the tool and returns what it answered, as a Result.
func (f *Functions) result(ctx context.Context, e eventsource.BedrockAgentFunctionEvent) (Result[any], error) {
	value, err := f.call(ctx, e)
	if err != nil {
		return Result[any]{}, err
	}
	r, ok := value.(result)
	if !ok {
		return Result[any]{Body: value}, nil
	}
	out := r.parts()
	if out.ResponseState != "" && out.ResponseState != Failure && out.ResponseState != Reprompt {
		return Result[any]{}, fmt.Errorf("%w: %q", ErrInvalidResponseState, out.ResponseState)
	}
	return out, nil
}

func (f *Functions) call(ctx context.Context, e eventsource.BedrockAgentFunctionEvent) (any, error) {
	t, ok := f.tools[e.Function]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownFunction, e.Function)
	}
	params := make(map[string]jsontext.Value, len(e.Parameters))
	for _, p := range e.Parameters {
		params[p.Name] = functionValue(p)
	}
	data, err := json.Marshal(params, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode the parameters: %w", err)
	}
	return t.call(ctx, data)
}

// text renders the body: a string as it is (Python JSON-quotes it), nothing
// as "", anything else as JSON.
func (f *Functions) text(body any) (string, error) {
	switch b := body.(type) {
	case nil:
		return "", nil
	case string:
		return b, nil
	default:
		v := reflect.ValueOf(body)
		if v.Kind() == reflect.Pointer && v.IsNil() {
			return "", nil
		}
		data, err := f.serializer(body)
		if err != nil {
			return "", fmt.Errorf("encode the body: %w", err)
		}
		return string(data), nil
	}
}

// functionValue converts a parameter as Python does: integers and numbers
// parsed (left as strings when they do not parse), booleans true only for
// "true"; arrays decoded when they are JSON, strings otherwise.
func functionValue(p eventsource.BedrockAgentProperty) jsontext.Value {
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
		if strings.EqualFold(p.Value, "true") {
			return jsontext.Value("true")
		}
		return jsontext.Value("false")
	case "array":
		if v := jsontext.Value(p.Value); v.IsValid() {
			return v
		}
	}
	v, err := json.Marshal(p.Value)
	if err != nil {
		return jsontext.Value(`""`)
	}
	return v
}

// typeName names the type of err the way Python names the exception class:
// without the package and the pointer.
func typeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() == "" {
		return t.String()
	}
	return t.Name()
}

func firstNonEmpty(maps ...map[string]string) map[string]string {
	for _, m := range maps {
		if len(m) > 0 {
			return m
		}
	}
	return map[string]string{}
}
