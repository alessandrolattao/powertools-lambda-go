package bedrockagent_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/bedrockagent"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

type vacation struct {
	Start string `json:"start_date"`
	End   string `json:"end_date"`
}

type typed struct {
	Count  int      `json:"count"`
	Ratio  float64  `json:"ratio"`
	OK     bool     `json:"ok"`
	Items  []string `json:"items"`
	Broken string   `json:"broken"`
}

var errNoBalance = errors.New("no balance left")

func TestFunctions(t *testing.T) {
	t.Parallel()
	fns := bedrockagent.NewFunctions()
	bedrockagent.Tool(fns, "submitVacationRequest", func(ctx context.Context, in vacation) (bedrockagent.Result[map[string]string], error) {
		e, ok := bedrockagent.FunctionEventFromContext(ctx)
		require.True(t, ok)
		return bedrockagent.Result[map[string]string]{
			Body:              map[string]string{"from": in.Start, "to": in.End, "employee": e.SessionAttributes["employeeId"]},
			SessionAttributes: map[string]string{"requested": "yes"},
			ResponseState:     bedrockagent.Reprompt,
			KnowledgeBases:    []bedrockagent.KnowledgeBaseConfiguration{{KnowledgeBaseID: "kb"}},
		}, nil
	}, bedrockagent.Description("Submits a vacation request"))

	e := load[eventsource.BedrockAgentFunctionEvent](t, "bedrockAgentFunctionEvent.json")
	resp, err := fns.Handle(t.Context(), e)
	require.NoError(t, err)
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"messageVersion": "1.0",
		"response": {
			"actionGroup": "VacationsActionGroup",
			"function": "submitVacationRequest",
			"functionResponse": {
				"responseBody": {"TEXT": {"body": "{\"employee\":\"EMP123\",\"from\":\"2024-03-15\",\"to\":\"2024-03-20\"}"}},
				"responseState": "REPROMPT"
			}
		},
		"sessionAttributes": {"requested": "yes"},
		"promptSessionAttributes": {"lastInteraction": "2024-02-01T15:30:00Z", "requestType": "vacation"},
		"knowledgeBasesConfiguration": [{"knowledgeBaseId": "kb"}]
	}`, string(data))
}

func TestFunctionValuesAndBodies(t *testing.T) {
	t.Parallel()
	fns := bedrockagent.NewFunctions()
	var got typed
	bedrockagent.Tool(fns, "typed", func(_ context.Context, in typed) (string, error) {
		got = in
		return "plain text", nil
	})
	bedrockagent.Tool(fns, "fail", func(context.Context, struct{}) (string, error) { return "", errNoBalance })
	bedrockagent.Tool(fns, "nil", func(context.Context, struct{}) (*int, error) { return nil, nil }) //nolint:nilnil // a nil body is the case under test
	bedrockagent.Tool(fns, "badState", func(context.Context, struct{}) (bedrockagent.Result[string], error) {
		return bedrockagent.Result[string]{Body: "x", ResponseState: "MAYBE"}, nil
	})
	bedrockagent.Tool(fns, "badParams", func(context.Context, struct {
		N int `json:"n"`
	},
	) (string, error) {
		return "", nil
	})

	call := func(function string, params ...eventsource.BedrockAgentProperty) bedrockagent.FunctionResponse {
		resp, err := fns.Handle(t.Context(), eventsource.BedrockAgentFunctionEvent{ActionGroup: "g", Function: function, Parameters: params})
		require.NoError(t, err, "failures never fail the invocation")
		return resp
	}
	body := func(r bedrockagent.FunctionResponse) string {
		return r.Response.FunctionResponse.ResponseBody["TEXT"].Body
	}

	resp := call("typed",
		eventsource.BedrockAgentProperty{Name: "count", Type: "integer", Value: "3"},
		eventsource.BedrockAgentProperty{Name: "ratio", Type: "number", Value: "0.5"},
		eventsource.BedrockAgentProperty{Name: "ok", Type: "boolean", Value: "True"},
		eventsource.BedrockAgentProperty{Name: "items", Type: "array", Value: `["a","b"]`},
		eventsource.BedrockAgentProperty{Name: "broken", Type: "integer", Value: "x"},
		eventsource.BedrockAgentProperty{Name: "extra", Type: "string", Value: "ignored"},
	)
	assert.Equal(t, typed{Count: 3, Ratio: 0.5, OK: true, Items: []string{"a", "b"}, Broken: "x"}, got)
	assert.Equal(t, "plain text", body(resp), "strings are sent as they are")
	assert.Equal(t, map[string]string{}, resp.SessionAttributes)
	assert.Equal(t, map[string]string{}, resp.PromptSessionAttributes)

	assert.Equal(t, "Error: errorString: no balance left", body(call("fail")))
	assert.Empty(t, body(call("nil")))
	assert.Equal(t, `Error: wrapError: responseState must be 'FAILURE' or 'REPROMPT': "MAYBE"`, body(call("badState")))
	assert.Equal(t, `Error: wrapError: unknown function: "missing"`, body(call("missing")))
	assert.Contains(t, body(call("badParams", eventsource.BedrockAgentProperty{Name: "n", Type: "string", Value: "x"})), "Error: ")

	custom := bedrockagent.NewFunctions(bedrockagent.WithSerializer(func(any) ([]byte, error) { return nil, errors.New("no") }))
	bedrockagent.Tool(custom, "x", func(context.Context, struct{}) (int, error) { return 1, nil })
	r, err := custom.Handle(t.Context(), eventsource.BedrockAgentFunctionEvent{Function: "x"})
	require.NoError(t, err)
	assert.Contains(t, r.Response.FunctionResponse.ResponseBody["TEXT"].Body, "Error: ")

	assert.Panics(t, func() {
		bedrockagent.Tool(fns, "typed", func(context.Context, struct{}) (string, error) { return "", nil })
	})
	_, ok := bedrockagent.FunctionEventFromContext(t.Context())
	assert.False(t, ok)
}

func TestIntegersAreNormalized(t *testing.T) {
	t.Parallel()
	fns := bedrockagent.NewFunctions()
	bedrockagent.Tool(fns, "n", func(_ context.Context, in typed) (int, error) { return in.Count, nil })
	resp, err := fns.Handle(t.Context(), eventsource.BedrockAgentFunctionEvent{Function: "n", Parameters: []eventsource.BedrockAgentProperty{
		{Name: "count", Type: "integer", Value: "+007"},
	}})
	require.NoError(t, err)
	assert.Equal(t, "7", resp.Response.FunctionResponse.ResponseBody["TEXT"].Body)
}
