package bedrockagent_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	jsv "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/bedrockagent"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func load[E any](t *testing.T, name string) E {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var e E
	require.NoError(t, json.Unmarshal(data, &e))
	return e
}

type Claim struct {
	ID     string `json:"id" required:"true"`
	Status string `json:"status" enum:"open,closed"`
}

type GetClaim struct {
	ClaimID string `path:"claim_id" required:"true" description:"The claim"`
}

type Reminder struct {
	ClaimID          int      `json:"claimId" required:"true"`
	PendingDocuments string   `json:"pendingDocuments" required:"true"`
	Urgent           bool     `json:"urgent"`
	Channels         []string `json:"channels"`
	Previous         *Claim   `json:"previous"`
}

func agentApp() *eventhandler.Router {
	app := eventhandler.New(eventhandler.WithDebug(false))
	eventhandler.Get(app, "/claims", func(ctx context.Context, _ struct{}) ([]Claim, error) {
		bedrockagent.SetSessionAttributes(ctx, map[string]string{"seen": "claims"})
		bedrockagent.SetPromptSessionAttributes(ctx, map[string]string{"step": "1"})
		bedrockagent.SetKnowledgeBasesConfiguration(ctx, []bedrockagent.KnowledgeBaseConfiguration{{KnowledgeBaseID: "kb"}})
		e, _ := bedrockagent.EventFromContext(ctx)
		return []Claim{{ID: e.SessionID, Status: "open"}}, nil
	}, eventhandler.Description("List the open claims"))
	eventhandler.Get(app, "/claims/{claim_id}", func(_ context.Context, in GetClaim) (Claim, error) {
		if in.ClaimID == "404" {
			return Claim{}, eventhandler.NotFound("no such claim")
		}
		if in.ClaimID == "boom" {
			return Claim{}, errors.New("boom")
		}
		return Claim{ID: in.ClaimID, Status: "closed"}, nil
	}, eventhandler.Description("Get a claim"), bedrockagent.RequireConfirmation())
	eventhandler.Post(app, "/send-reminders", func(_ context.Context, in struct{ Body Reminder }) (Reminder, error) {
		return in.Body, nil
	}, eventhandler.Description("Send reminders"), eventhandler.StatusCode(http.StatusAccepted))
	return app
}

func TestAgentRoundTrip(t *testing.T) {
	t.Parallel()
	handler := bedrockagent.Handler(agentApp())

	resp, err := handler(t.Context(), load[eventsource.BedrockAgentEvent](t, "bedrockAgentEvent.json"))
	require.NoError(t, err)
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"messageVersion": "1.0",
		"response": {
			"actionGroup": "ClaimManagementActionGroup",
			"apiPath": "/claims",
			"httpMethod": "GET",
			"httpStatusCode": 200,
			"responseBody": {"application/json": {"body": "[{\"id\":\"12345678912345\",\"status\":\"open\"}]"}}
		},
		"sessionAttributes": {"seen": "claims"},
		"promptSessionAttributes": {"step": "1"},
		"knowledgeBasesConfiguration": [{"knowledgeBaseId": "kb"}]
	}`, string(data))

	resp, err = handler(t.Context(), load[eventsource.BedrockAgentEvent](t, "bedrockAgentEventWithPathParams.json"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.Response.HTTPStatusCode)
	assert.JSONEq(t, `{"id":"123","status":"closed"}`, resp.Response.ResponseBody["application/json"].Body)
	assert.Nil(t, resp.SessionAttributes, "only set attributes are sent")

	post := load[eventsource.BedrockAgentEvent](t, "bedrockAgentPostEvent.json")
	resp, err = handler(t.Context(), post)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Response.HTTPStatusCode, "claimId is a string the schema wants as integer")
	assert.JSONEq(t, `{"statusCode":422,"detail":[{"loc":["body","claimId"],"type":"int_parsing"}]}`, resp.Response.ResponseBody["application/json"].Body)

	media := post.RequestBody.Content["application/json"]
	media.Properties = append(media.Properties,
		eventsource.BedrockAgentProperty{Name: "urgent", Type: "boolean", Value: "TRUE"},
		eventsource.BedrockAgentProperty{Name: "channels", Type: "array", Value: `["sms","mail"]`})
	media.Properties[0].Type = "integer"
	post.RequestBody.Content["application/json"] = media
	resp, err = handler(t.Context(), post)
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, resp.Response.HTTPStatusCode)
	assert.JSONEq(t, `{"claimId":20,"pendingDocuments":"social number and vat","urgent":true,"channels":["sms","mail"],"previous":null}`,
		resp.Response.ResponseBody["application/json"].Body)
}

func TestAgentErrors(t *testing.T) {
	t.Parallel()
	handler := bedrockagent.Handler(agentApp())
	e := load[eventsource.BedrockAgentEvent](t, "bedrockAgentEventWithPathParams.json")

	e.Parameters[0].Value = "404"
	resp, err := handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.Response.HTTPStatusCode)
	assert.JSONEq(t, `{"statusCode":404,"message":"no such claim"}`, resp.Response.ResponseBody["application/json"].Body)

	e.Parameters[0].Value = "boom"
	_, err = handler(t.Context(), e)
	require.EqualError(t, err, "boom", "an unhandled error fails the invocation, as in Python")

	e.APIPath = "/unknown"
	resp, err = handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.Response.HTTPStatusCode)

	_, ok := bedrockagent.EventFromContext(t.Context())
	assert.False(t, ok)
	bedrockagent.SetSessionAttributes(t.Context(), map[string]string{"x": "y"}) // outside an invocation: nothing happens
}

func TestAgentTextResponse(t *testing.T) {
	t.Parallel()
	app := eventhandler.New()
	app.HandleFunc("GET /claims", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := w.Write([]byte("You have 3 claims"))
		if err != nil {
			panic(err)
		}
	})
	resp, err := bedrockagent.Handler(app)(t.Context(), load[eventsource.BedrockAgentEvent](t, "bedrockAgentEvent.json"))
	require.NoError(t, err)
	assert.Equal(t, map[string]bedrockagent.ResponseContent{"text/plain": {Body: "You have 3 claims"}}, resp.Response.ResponseBody)
}

func TestAgentOpenAPI(t *testing.T) {
	t.Parallel()
	doc, err := bedrockagent.OpenAPIJSON(agentApp())
	require.NoError(t, err)
	path := filepath.Join("testdata", "agent-openapi.json")
	if *update {
		require.NoError(t, os.WriteFile(path, append(doc, '\n'), 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(bytes.TrimSpace(want)), string(doc))

	f, err := os.Open(filepath.Join("testdata", "openapi-3.0-schema.json"))
	require.NoError(t, err)
	schema, err := jsv.UnmarshalJSON(f)
	require.NoError(t, f.Close())
	require.NoError(t, err)
	c := jsv.NewCompiler()
	require.NoError(t, c.AddResource("openapi-3.0.json", schema))
	sch, err := c.Compile("openapi-3.0.json")
	require.NoError(t, err)
	instance, err := jsv.UnmarshalJSON(bytes.NewReader(doc))
	require.NoError(t, err)
	require.NoError(t, sch.Validate(instance))

	doc, err = bedrockagent.OpenAPIJSON(agentApp(), func(c *eventhandler.OpenAPIConfig) { c.Version = "2.0.0" })
	require.NoError(t, err)
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(doc, &spec))
	assert.Equal(t, "3.0.3", spec.OpenAPI)
	assert.Equal(t, bedrockagent.DefaultTitle, spec.Info.Title)
	assert.Equal(t, "2.0.0", spec.Info.Version)
}

func TestAgentOpenAPIRequirements(t *testing.T) {
	t.Parallel()
	noDescription := eventhandler.New()
	noDescription.HandleFunc("GET /x", func(http.ResponseWriter, *http.Request) {})
	_, err := bedrockagent.OpenAPIJSON(noDescription)
	require.ErrorIs(t, err, bedrockagent.ErrMissingDescription)

	secured := eventhandler.New()
	secured.HandleFunc("GET /x", func(http.ResponseWriter, *http.Request) {},
		eventhandler.Description("x"), eventhandler.Security(eventhandler.SecurityRequirement{"a": nil}))
	_, err = bedrockagent.OpenAPIJSON(secured)
	require.ErrorIs(t, err, bedrockagent.ErrSecurityUnsupported)
}
