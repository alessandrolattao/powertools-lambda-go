package eventhandler

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"flag"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	jsv "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// golden compares data with the golden file name, or rewrites it with
// -update.
func golden(t *testing.T, name string, data []byte) {
	t.Helper()
	path := filepath.Join("testdata", "openapi", name)
	if *update {
		require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(bytes.TrimSpace(want)), string(bytes.TrimSpace(data)))
}

// validateOpenAPI31 validates doc against the OpenAPI 3.1 schema, schema
// objects included.
func validateOpenAPI31(t *testing.T, doc []byte) {
	t.Helper()
	c := jsv.NewCompiler()
	for _, name := range []string{"openapi-3.1-schema.json", "openapi-3.1-schema-base.json", "openapi-3.1-dialect-base.json", "openapi-3.1-meta-base.json"} {
		f, err := os.Open(filepath.Join("testdata", "openapi", name))
		require.NoError(t, err)
		schema, err := jsv.UnmarshalJSON(f)
		require.NoError(t, f.Close())
		require.NoError(t, err)
		m, ok := schema.(map[string]any)
		require.True(t, ok)
		id, ok := m["$id"].(string)
		require.True(t, ok)
		require.NoError(t, c.AddResource(id, schema))
	}
	sch, err := c.Compile("https://spec.openapis.org/oas/3.1/schema-base/2022-10-07")
	require.NoError(t, err)
	instance, err := jsv.UnmarshalJSON(bytes.NewReader(doc))
	require.NoError(t, err)
	require.NoError(t, sch.Validate(instance))
}

type Todo struct {
	ID        int       `json:"id" required:"true" description:"The todo id"`
	Title     string    `json:"title" required:"true" minLength:"1" maxLength:"200" example:"Buy milk"`
	Completed bool      `json:"completed"`
	Due       time.Time `json:"due,omitzero"`
	Labels    []string  `json:"labels,omitempty" uniqueItems:"true"`
}

type ListTodosInput struct {
	Limit     int    `query:"limit" default:"10" minimum:"1" maximum:"100" description:"Page size"`
	Completed *bool  `query:"completed"`
	Sort      string `query:"sort" enum:"asc,desc" deprecated:"true"`
	TraceID   string `header:"X-Trace-Id" required:"true"`
	Session   string `cookie:"session"`
}

type GetTodoInput struct {
	ID int `path:"id" minimum:"1"`
}

type CreateTodoInput struct {
	Body Todo
}

type UploadInput struct {
	Name string                `formData:"name" required:"true"`
	File *multipart.FileHeader `formData:"file" required:"true"`
}

type ErrorBody struct {
	Message string `json:"message"`
}

func sampleApp() *Router {
	app := New(WithOpenAPI(OpenAPIConfig{
		Title:          "Todos API",
		Version:        "2.0.0",
		Summary:        "Manage todos",
		Description:    "An API to **manage** todos.",
		TermsOfService: "https://example.com/terms",
		Contact:        &Contact{Name: "Team", URL: "https://example.com", Email: "team@example.com"},
		License:        &License{Name: "MIT", Identifier: "MIT"},
		Servers: []Server{{
			URL: "https://{env}.example.com", Description: "Environments",
			Variables: map[string]ServerVariable{"env": {Enum: []string{"dev", "prod"}, Default: "dev"}},
		}},
		Tags: []Tag{{Name: "todos", Description: "Todo items", ExternalDocs: &ExternalDocs{URL: "https://example.com/todos"}}},
		SecuritySchemes: map[string]SecurityScheme{
			"apiKey": APIKeyScheme("X-Api-Key", "header"),
			"bearer": HTTPBearerScheme("JWT"),
			"oauth": OAuth2Scheme(OAuthFlows{AuthorizationCode: &OAuthFlow{
				AuthorizationURL: "https://auth.example.com/authorize", TokenURL: "https://auth.example.com/token",
				Scopes: map[string]string{"todos:write": "Write todos"},
			}}),
			"oidc":  OpenIDConnectScheme("https://auth.example.com/.well-known/openid-configuration"),
			"basic": HTTPScheme("basic"),
			"mtls":  MutualTLSScheme(),
		},
		Security:     []SecurityRequirement{{"apiKey": nil}},
		ExternalDocs: &ExternalDocs{Description: "Guide", URL: "https://example.com/docs"},
		Extensions:   map[string]any{"x-owner": "team"},
	}))
	Get(app, "/todos", func(context.Context, ListTodosInput) ([]Todo, error) { return nil, nil },
		Tags("todos"), Summary("List todos"))
	Get(app, "/todos/{id}", func(context.Context, GetTodoInput) (Todo, error) { return Todo{}, nil },
		Tags("todos"), Description("Get one todo"), OperationID("getTodo"),
		ResponseModel(http.StatusOK, "The todo", nil), ResponseModel(http.StatusNotFound, "No such todo", ErrorBody{}))
	Post(app, "/todos", func(context.Context, CreateTodoInput) (Response[Todo], error) { return Response[Todo]{}, nil },
		Tags("todos"), StatusCode(http.StatusCreated), ResponseDescription("Created"),
		Security(SecurityRequirement{"oauth": {"todos:write"}}), Extension("x-internal", true), ResponseValidation(http.StatusInternalServerError))
	Delete(app, "/todos/{id}", func(context.Context, GetTodoInput) (struct{}, error) { return struct{}{}, nil },
		Deprecated(), StatusCode(http.StatusNoContent))
	Post(app, "/upload", func(context.Context, UploadInput) (string, error) { return "", nil })
	app.HandleFunc("GET /hello/{name}", func(http.ResponseWriter, *http.Request) {})
	app.HandleFunc("GET /files/{path...}", func(http.ResponseWriter, *http.Request) {}, Security(SecurityRequirement{}))
	app.HandleFunc("GET /hidden", func(http.ResponseWriter, *http.Request) {}, ExcludeFromSchema())
	app.HandleFunc("/any", func(http.ResponseWriter, *http.Request) {})
	app.HandleRegexp(http.MethodGet, regexp.MustCompile(`/re/.+`), http.NotFoundHandler())
	return app
}

func TestOpenAPIGolden(t *testing.T) {
	t.Parallel()
	doc, err := sampleApp().OpenAPIJSON()
	require.NoError(t, err)
	golden(t, "todos.json", doc)
	validateOpenAPI31(t, doc)

	// The typed document encodes to the same JSON: no member lost, none
	// repeated (encoding/json/v2 rejects duplicate names).
	spec, err := sampleApp().OpenAPI()
	require.NoError(t, err)
	data, err := spec.MarshalJSON()
	require.NoError(t, err)
	var fromSpec, fromJSON any
	require.NoError(t, json.Unmarshal(data, &fromSpec))
	require.NoError(t, json.Unmarshal(doc, &fromJSON))
	assert.Equal(t, fromJSON, fromSpec)
}

func TestOpenAPIDefaults(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) {})
	spec, err := app.OpenAPI()
	require.NoError(t, err)
	assert.Equal(t, "3.1.0", spec.Openapi)
	assert.Equal(t, DefaultOpenAPITitle, spec.Info.Title)
	assert.Equal(t, DefaultAPIVersion, spec.Info.Version)
	require.Len(t, spec.Servers, 1)
	assert.Equal(t, "/", spec.Servers[0].URL)
	op := spec.Paths.MapOfPathItemValues["/"].Get
	require.NotNil(t, op)
	assert.Equal(t, "GET /", *op.Summary)
	assert.Equal(t, "_get", *op.ID)
	require.NotNil(t, spec.Components)
	assert.Contains(t, spec.Components.Schemas, "HTTPValidationError", "always present, as in Python")

	doc, err := app.OpenAPIJSON(func(c *OpenAPIConfig) { c.Title = "Override" })
	require.NoError(t, err)
	assert.Contains(t, string(doc), `"title": "Override"`)
	validateOpenAPI31(t, doc)
}

func TestOperationIDFromFunctionName(t *testing.T) {
	t.Parallel()
	app := New()
	Get(app, "/todos/{id}", getTodoTitle)
	app.HandleFunc("GET /plain", plainHandler)
	spec, err := app.OpenAPI()
	require.NoError(t, err)
	assert.Equal(t, "getTodoTitle_todos__id__get", *spec.Paths.MapOfPathItemValues["/todos/{id}"].Get.ID)
	assert.Equal(t, "plainHandler_plain_get", *spec.Paths.MapOfPathItemValues["/plain"].Get.ID)
}

func getTodoTitle(context.Context, GetTodoInput) (string, error) { return "", nil }

func plainHandler(http.ResponseWriter, *http.Request) {}

func TestOpenAPIConfigurationErrors(t *testing.T) {
	t.Parallel()
	noSchemes := New()
	noSchemes.HandleFunc("GET /", plainHandler, Security(SecurityRequirement{"missing": nil}))
	_, err := noSchemes.OpenAPI()
	require.ErrorIs(t, err, ErrSchemaValidation)

	unknown := New(WithOpenAPI(OpenAPIConfig{SecuritySchemes: map[string]SecurityScheme{"a": HTTPScheme("basic")}}))
	unknown.HandleFunc("GET /", plainHandler, Security(SecurityRequirement{"b": nil}))
	_, err = unknown.OpenAPI()
	require.ErrorIs(t, err, ErrSchemaValidation)

	global := New(WithOpenAPI(OpenAPIConfig{Security: []SecurityRequirement{{"x": nil}}}))
	_, err = global.OpenAPI()
	require.ErrorIs(t, err, ErrSchemaValidation)

	ext := New()
	ext.HandleFunc("GET /", plainHandler, Extension("bad", 1))
	_, err = ext.OpenAPI()
	require.ErrorIs(t, err, ErrSchemaValidation)

	docExt := New(WithOpenAPI(OpenAPIConfig{Extensions: map[string]any{"owner": "x"}}))
	_, err = docExt.OpenAPI()
	require.ErrorIs(t, err, ErrSchemaValidation)

	badScheme := New(WithOpenAPI(OpenAPIConfig{SecuritySchemes: map[string]SecurityScheme{"x": {Type: "magic"}}}))
	_, err = badScheme.OpenAPI()
	require.ErrorIs(t, err, ErrSchemaValidation)

	mtls := New(WithOpenAPI(OpenAPIConfig{SecuritySchemes: map[string]SecurityScheme{"m": MutualTLSScheme()}}))
	_, err = mtls.OpenAPI30JSON()
	require.ErrorIs(t, err, ErrSchemaValidation, "mutual TLS needs 3.1")
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	routes := sampleApp().Routes()
	patterns := make([]string, 0, len(routes))
	for _, r := range routes {
		patterns = append(patterns, r.Pattern)
	}
	assert.Equal(t, []string{
		"GET /todos", "GET /todos/{id}", "POST /todos", "DELETE /todos/{id}", "POST /upload",
		"GET /hello/{name}", "GET /files/{path...}",
	}, patterns)
}

func TestMergeOpenAPI(t *testing.T) {
	t.Parallel()
	users := New()
	Get(users, "/users/{id}", func(context.Context, GetTodoInput) (Todo, error) { return Todo{}, nil }, Tags("users"))
	app := New()
	Get(app, "/todos", func(context.Context, ListTodosInput) ([]Todo, error) { return nil, nil }, Tags("todos"))
	conflict := New()
	conflict.HandleFunc("GET /todos", plainHandler, Summary("Other"))

	docs := make([][]byte, 0, 3)
	for _, r := range []*Router{app, users, conflict} {
		doc, err := r.OpenAPIJSON()
		require.NoError(t, err)
		docs = append(docs, doc)
	}
	cfg := OpenAPIConfig{Title: "Merged", Tags: []Tag{{Name: "users", Description: "People"}}}
	merged, err := MergeOpenAPI(cfg, ConflictWarn, docs...)
	require.NoError(t, err)
	golden(t, "merged.json", merged)
	validateOpenAPI31(t, merged)

	last, err := MergeOpenAPI(cfg, ConflictLast, docs...)
	require.NoError(t, err)
	assert.Contains(t, string(last), `"summary": "Other"`)
	first, err := MergeOpenAPI(cfg, ConflictFirst, docs...)
	require.NoError(t, err)
	assert.NotContains(t, string(first), `"summary": "Other"`)
	_, err = MergeOpenAPI(cfg, ConflictError, docs...)
	require.ErrorIs(t, err, ErrMergeConflict)
	_, err = MergeOpenAPI(cfg, ConflictWarn, []byte("not json"))
	require.Error(t, err)
}
