package validation

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

type order struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type response struct {
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body,omitzero"`
	Version    string `json:"version,omitzero"`
	internal   string
}

const (
	inboundSchema  = `{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"status":{"default":"pending"}}}`
	outboundSchema = `{"type":"object","required":["statusCode"],"properties":{"statusCode":{"maximum":299},"version":{"default":"1.0"}}}`
)

func mustJSON(t *testing.T, v any) jsontext.Value {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func TestValidatorInboundAndOutbound(t *testing.T) {
	t.Parallel()
	var got order
	h := Validator(func(_ context.Context, o order) (response, error) {
		got = o
		return response{StatusCode: 200, internal: "kept"}, nil
	}, Inbound(inboundSchema), Outbound(outboundSchema))

	resp, err := h(t.Context(), jsontext.Value(`{"id":"o-1"}`))
	require.NoError(t, err)
	assert.Equal(t, order{ID: "o-1", Status: "pending"}, got, "the handler sees the inbound defaults")
	assert.Equal(t, response{StatusCode: 200, Version: "1.0", internal: "kept"}, resp, "the response gets the outbound defaults")
}

func TestValidatorRejectsInbound(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	h := Validator(func(context.Context, order) (response, error) {
		called.Store(true)
		return response{}, nil
	}, Inbound(inboundSchema))

	_, err := h(t.Context(), jsontext.Value(`{"status":"x"}`))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data must contain ['id'] properties", verr.ValidationMessage)
	assert.False(t, called.Load())
}

func TestValidatorRejectsOutbound(t *testing.T) {
	t.Parallel()
	h := Validator(func(context.Context, map[string]any) (response, error) {
		return response{StatusCode: 500}, nil
	}, Outbound(outboundSchema))

	_, err := h(t.Context(), jsontext.Value(`{}`))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data.statusCode must be smaller than or equal to 299", verr.ValidationMessage)
}

func TestValidatorEnvelope(t *testing.T) {
	t.Parallel()
	event := mustJSON(t, events.SQSEvent{Records: []events.SQSMessage{{Body: `{"id":"a"}`}, {Body: `{"id":"b","status":"done"}`}}})
	var got []order
	h := Validator(func(_ context.Context, orders []order) (string, error) {
		got = orders
		return "ok", nil
	}, Inbound(`{"type":"array","items":`+inboundSchema+`}`, WithEnvelope(SQS)))

	out, err := h(t.Context(), event)
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
	assert.Equal(t, []order{{ID: "a", Status: "pending"}, {ID: "b", Status: "done"}}, got)
}

func TestValidatorEnvelopeWithoutSchema(t *testing.T) {
	t.Parallel()
	event := mustJSON(t, events.EventBridgeEvent{DetailType: "OrderPlaced", Detail: jsontext.Value(`{"id":"o-9"}`)})
	h := Validator(func(_ context.Context, o order) (string, error) { return o.ID, nil }, Inbound(nil, WithEnvelope(EventBridge)))
	out, err := h(t.Context(), event)
	require.NoError(t, err)
	assert.Equal(t, "o-9", out)
}

func TestValidatorRawHandlers(t *testing.T) {
	t.Parallel()
	h := Validator(func(_ context.Context, event map[string]any) (map[string]any, error) {
		return map[string]any{"statusCode": 201, "echo": event["status"]}, nil
	}, Inbound(inboundSchema), Outbound(outboundSchema))
	resp, err := h(t.Context(), jsontext.Value(`{"id":"x"}`))
	require.NoError(t, err)
	assert.Equal(t, "1.0", resp["version"])
	assert.Equal(t, "pending", resp["echo"])

	raw := Validator(func(_ context.Context, event jsontext.Value) (jsontext.Value, error) { return event, nil })
	out, err := raw(t.Context(), jsontext.Value(`{"a":1}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(out))
}

func TestValidatorOutboundEnvelope(t *testing.T) {
	t.Parallel()
	h := Validator(func(context.Context, any) (events.APIGatewayProxyResponse, error) {
		return events.APIGatewayProxyResponse{StatusCode: 200, Body: `{"id":1}`}, nil
	}, Outbound(`{"properties":{"id":{"type":"string"},"x":{"default":1}}}`, WithEnvelope("powertools_json(body)")))
	_, err := h(t.Context(), jsontext.Value(`{}`))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data.id must be string", verr.ValidationMessage)
}

func TestValidatorHandlerError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	h := Validator(func(context.Context, order) (response, error) {
		return response{StatusCode: 999}, boom
	}, Outbound(outboundSchema))
	resp, err := h(t.Context(), jsontext.Value(`{}`))
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 999, resp.StatusCode, "the outbound schema is not checked on failure")
}

func TestValidatorDecodeErrors(t *testing.T) {
	t.Parallel()
	h := Validator(func(context.Context, order) (string, error) { return "", nil })
	_, err := h(t.Context(), jsontext.Value(`{"id":1}`))
	require.Error(t, err)

	withSchema := Validator(func(context.Context, order) (string, error) { return "", nil }, Inbound(`{}`))
	_, err = withSchema(t.Context(), jsontext.Value(`{"id":1}`))
	require.Error(t, err)

	withDefaults := Validator(func(context.Context, order) (string, error) { return "", nil },
		Inbound(`{"properties":{"status":{"default":1}}}`))
	_, err = withDefaults(t.Context(), jsontext.Value(`{}`))
	require.Error(t, err)

	notJSON := Validator(func(context.Context, any) (chan int, error) { return make(chan int), nil }, Outbound(`{}`))
	_, err = notJSON(t.Context(), jsontext.Value(`{}`))
	require.Error(t, err)

	badEvent := Validator(func(context.Context, any) (string, error) { return "", nil }, Inbound(`{}`))
	_, err = badEvent(t.Context(), jsontext.Value(`{`))
	require.ErrorIs(t, err, ErrInvalidEvent)
}

func TestValidatorRetriesSchemaCompilation(t *testing.T) {
	t.Parallel()
	var available atomic.Bool
	load := func(context.Context, string) (any, error) {
		if !available.Load() {
			return nil, errors.New("unreachable")
		}
		return childSchema, nil
	}
	h := Validator(func(context.Context, any) (string, error) { return "ok", nil },
		Inbound(parentSchema, WithLoader("https", load)))

	_, err := h(t.Context(), jsontext.Value(`{"ParentSchema":{"project":"p"}}`))
	require.ErrorIs(t, err, ErrInvalidSchema)

	available.Store(true)
	out, err := h(t.Context(), jsontext.Value(`{"ParentSchema":{"project":"p"}}`))
	require.NoError(t, err)
	assert.Equal(t, "ok", out)

	available.Store(false)
	out, err = h(t.Context(), jsontext.Value(`{"ParentSchema":{"project":"p"}}`))
	require.NoError(t, err, "a compiled schema is kept")
	assert.Equal(t, "ok", out)
}

func TestValidatorEnvelopeCompileError(t *testing.T) {
	t.Parallel()
	h := Validator(func(context.Context, any) (string, error) { return "", nil }, Inbound(nil, WithEnvelope("a[")))
	_, err := h(t.Context(), jsontext.Value(`{}`))
	require.ErrorIs(t, err, jmespath.ErrInvalidEnvelope)

	out := Validator(func(context.Context, any) (string, error) { return "", nil }, Outbound(`{"type":5}`))
	_, err = out(t.Context(), jsontext.Value(`{}`))
	require.ErrorIs(t, err, ErrInvalidSchema)
}

func TestValidatorConcurrentInvocations(t *testing.T) {
	t.Parallel()
	h := Validator(func(_ context.Context, o order) (response, error) {
		return response{StatusCode: 200, Body: o.ID}, nil
	}, Inbound(inboundSchema), Outbound(outboundSchema))
	type outcome struct {
		resp response
		err  error
	}
	results := make(chan outcome, 16)
	for range 16 {
		go func() {
			resp, err := h(t.Context(), jsontext.Value(`{"id":"c"}`))
			results <- outcome{resp, err}
		}()
	}
	for range 16 {
		got := <-results
		require.NoError(t, got.err)
		assert.Equal(t, "c", got.resp.Body)
	}
}
