package schemavalidation_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/schemavalidation"
)

// do serves one request with app and returns the recorded response.
func do(t *testing.T, app http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, reader))
	return rec
}

// decode decodes a JSON body.
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &m), body)
	return m
}

const inboundSchema = `{
	"type": "object",
	"properties": {"message": {"type": "string"}, "username": {"type": "string", "maxLength": 5}},
	"required": ["message"]
}`

const outboundSchema = `{"type": "object", "required": ["ok"]}`

func TestSchemaValidationMiddleware(t *testing.T) {
	t.Parallel()
	app := eventhandler.New()
	echoBody := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		if _, err := w.Write(data); err != nil {
			panic(err)
		}
	})
	app.Handle("POST /in", echoBody, eventhandler.Middlewares(schemavalidation.Middleware(inboundSchema, nil)))
	app.Handle("POST /out", echoBody, eventhandler.Middlewares(schemavalidation.Middleware(inboundSchema, outboundSchema)))
	app.Handle("POST /broken", echoBody, eventhandler.Middlewares(schemavalidation.Middleware(`{"type": 12}`, nil)))

	rec := do(t, app, "/in", `{"message":"hi","username":"tom"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"message":"hi","username":"tom"}`, rec.Body.String(), "the handler reads the body again")

	rec = do(t, app, "/in", `{"message":"hi","username":"tommaso"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	body := decode(t, rec.Body.String())
	assert.InDelta(t, 400, body["statusCode"], 0)
	message, ok := body["message"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(message, "Bad Request: Failed schema validation. Error: data.username must be shorter than or equal to 5 characters"),
		message)

	assert.Equal(t, http.StatusBadRequest, do(t, app, "/in", "").Code, "an empty body is null, which is not an object")
	assert.Equal(t, http.StatusBadRequest, do(t, app, "/in", "{").Code)

	rec = do(t, app, "/out", `{"message":"hi"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Bad Response: ")
	assert.Equal(t, http.StatusOK, do(t, app, "/out", `{"message":"hi","ok":true}`).Code)

	rec = do(t, app, "/broken", `{}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"statusCode":500,"message":"Internal Server Error"}`, rec.Body.String())
}
