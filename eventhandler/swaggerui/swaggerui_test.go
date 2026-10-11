package swaggerui_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/swaggerui"
)

func newApp() *eventhandler.Router {
	app := eventhandler.New(eventhandler.WithOpenAPI(eventhandler.OpenAPIConfig{Title: "Todos", Description: "</script><b>"}))
	eventhandler.Get(app, "/todos", func(context.Context, struct {
		Limit int `query:"limit"`
	},
	) ([]string, error) {
		return nil, nil
	})
	return app
}

func get(t *testing.T, h http.Handler, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSwaggerUIPage(t *testing.T) {
	t.Parallel()
	app := newApp()
	swaggerui.Enable(app, swaggerui.WithPersistAuthorization(), swaggerui.WithOAuth2(swaggerui.OAuth2Config{
		ClientID: "client", AppName: "Todos", Scopes: []string{"read"}, UsePkceWithAuthorizationCodeGrant: true,
	}))

	rec := get(t, app, "/swagger")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, eventhandler.ContentTypeHTML, rec.Header().Get("Content-Type"))
	page := rec.Body.String()
	for _, want := range []string{
		"<title>Swagger UI</title>",
		"SwaggerUIBundle",
		".swagger-ui{",
		"persistAuthorization: true",
		`ui.initOAuth({"clientId":"client","appName":"Todos","scopes":["read"],"usePkceWithAuthorizationCodeGrant":true});`,
		`<\/script><b>`,
	} {
		assert.Contains(t, page, want)
	}
	assert.NotContains(t, page, "</script><b>", "the document cannot close the script it is inlined into")
	assert.Greater(t, len(page), 1_500_000, "the assets are inlined")

	doc := get(t, app, "/swagger?format=json")
	assert.Equal(t, eventhandler.ContentTypeJSON, doc.Header().Get("Content-Type"))
	var spec map[string]any
	require.NoError(t, json.Unmarshal(doc.Body.Bytes(), &spec))
	assert.Equal(t, []any{map[string]any{"url": "/"}}, spec["servers"])
	assert.NotContains(t, spec["paths"], "/swagger", "the UI is not part of the document")

	redirect := get(t, app, "/swagger?format=oauth2-redirect")
	assert.Equal(t, eventhandler.ContentTypeHTML, redirect.Header().Get("Content-Type"))
	assert.Contains(t, redirect.Body.String(), "swaggerUIRedirectOauth2")
}

func TestSwaggerUIOptions(t *testing.T) {
	t.Parallel()
	app := newApp()
	blocked := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			eventhandler.Error(w, r, eventhandler.Forbidden("no"))
		})
	}
	swaggerui.Enable(app, swaggerui.WithPath("/docs"), swaggerui.WithBaseURL("https://cdn.example.com/swagger/"), swaggerui.WithCompression())
	swaggerui.Enable(app, swaggerui.WithPath("/private"), swaggerui.WithMiddlewares(blocked))
	swaggerui.Enable(app, swaggerui.WithPath("/custom"), swaggerui.WithCSSURL("/s.css"), swaggerui.WithJSURL("/s.js?a=1&b='x'"),
		swaggerui.WithOpenAPI(func(c *eventhandler.OpenAPIConfig) {
			c.Servers = []eventhandler.Server{{URL: "https://api.example.com"}}
		}))
	swaggerui.Enable(app, swaggerui.WithPath("/merged"), swaggerui.WithDocument(func(*http.Request) ([]byte, error) {
		return []byte(`{"openapi":"3.1.0"}`), nil
	}))

	page := get(t, app, "/docs").Body.String()
	assert.Less(t, len(page), 10_000)
	assert.Contains(t, page, "<link rel='stylesheet' type='text/css' href='https://cdn.example.com/swagger/swagger-ui.min.css'>")
	assert.Contains(t, page, "<script src='https://cdn.example.com/swagger/swagger-ui-bundle.min.js'></script>")
	assert.Contains(t, page, "persistAuthorization: false")
	assert.Equal(t, "gzip", get(t, app, "/docs", "Accept-Encoding", "gzip").Header().Get("Content-Encoding"))

	assert.Equal(t, http.StatusForbidden, get(t, app, "/private").Code)

	custom := get(t, app, "/custom").Body.String()
	require.Less(t, len(custom), 10_000)
	assert.Contains(t, custom, "href='/s.css'")
	assert.Contains(t, custom, "src='/s.js?a=1&amp;b=&#39;x&#39;'")
	assert.Contains(t, get(t, app, "/custom?format=json").Body.String(), "https://api.example.com")

	assert.JSONEq(t, `{"openapi":"3.1.0"}`, get(t, app, "/merged?format=json").Body.String())
}

func TestSwaggerUIStageServer(t *testing.T) {
	t.Parallel()
	app := newApp()
	swaggerui.Enable(app)
	resp, err := eventhandler.APIGatewayREST(app)(t.Context(), events.APIGatewayProxyRequest{
		HTTPMethod: http.MethodGet, Path: "/swagger",
		QueryStringParameters: map[string]string{"format": "json"},
		RequestContext:        events.APIGatewayProxyRequestContext{Stage: "prod", Path: "/prod/swagger"},
	})
	require.NoError(t, err)
	assert.Contains(t, resp.Body, `"url": "/prod"`, "Try it out targets the stage")
}

func TestSwaggerUIClientSecret(t *testing.T) {
	cfg := swaggerui.OAuth2Config{AppName: "x", ClientSecret: "s3cr3t"}
	t.Setenv("POWERTOOLS_DEV", "false")
	assert.PanicsWithValue(t, swaggerui.ErrClientSecret, func() { swaggerui.Enable(newApp(), swaggerui.WithOAuth2(cfg)) })

	t.Setenv("POWERTOOLS_DEV", "true")
	app := newApp()
	assert.NotPanics(t, func() { swaggerui.Enable(app, swaggerui.WithOAuth2(cfg)) })
	assert.Contains(t, get(t, app, "/swagger").Body.String(), `"clientSecret":"s3cr3t"`)
}

func TestHandlerErrors(t *testing.T) {
	t.Parallel()
	app := eventhandler.New()
	app.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) {}, eventhandler.Security(eventhandler.SecurityRequirement{"missing": nil}))
	app.Handle("GET /swagger", swaggerui.Handler(app))
	rec := get(t, app, "/swagger")
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "a document that cannot be built is an error")
}
