package validation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The schemas of Powertools Python's custom handlers example.
const (
	parentSchema = `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"$id": "https://example.com/schemas/parent.json",
		"type": "object",
		"properties": {"ParentSchema": {"$ref": "https://SCHEMA"}}
	}`
	childSchema = `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"$id": "https://example.com/schemas/child.json",
		"type": "object",
		"properties": {"project": {"type": "string"}},
		"required": ["project"]
	}`
)

func TestRemoteReferencesNeedALoader(t *testing.T) {
	t.Parallel()
	_, err := Compile(parentSchema)
	require.ErrorIs(t, err, ErrInvalidSchema)
	require.ErrorIs(t, err, ErrNoLoader)
}

func TestLoader(t *testing.T) {
	t.Parallel()
	var requested []string
	schema, err := Compile(parentSchema, WithLoader("https", func(_ context.Context, url string) (any, error) {
		requested = append(requested, url)
		return childSchema, nil
	}))
	require.NoError(t, err)
	assert.Len(t, requested, 1)

	_, err = schema.Validate([]byte(`{"ParentSchema":{"project":"powertools"}}`))
	require.NoError(t, err)

	_, err = schema.Validate([]byte(`{"ParentSchema":{}}`))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data.ParentSchema must contain ['project'] properties", verr.ValidationMessage)
	assert.Equal(t, "required", verr.Rule)
	assert.Equal(t, "[project]", str(verr.RuleDefinition), "the definition comes from the loaded document")
}

func TestLoaderReturningAValue(t *testing.T) {
	t.Parallel()
	schema, err := Compile(`{"$ref":"urn:schemas:positive"}`, WithLoader("URN", func(context.Context, string) (any, error) {
		return map[string]any{"type": "number", "exclusiveMinimum": 0}, nil
	}))
	require.NoError(t, err)
	_, err = schema.Validate([]byte(`0`))
	require.ErrorIs(t, err, ErrSchemaValidation)
}

func TestLoaderErrors(t *testing.T) {
	t.Parallel()
	missing := errors.New("no such key")
	_, err := Compile(parentSchema, WithLoader("https", func(context.Context, string) (any, error) { return nil, missing }))
	require.ErrorIs(t, err, ErrInvalidSchema)
	require.ErrorIs(t, err, missing)

	_, err = Compile(parentSchema, WithLoader("https", func(context.Context, string) (any, error) { return "{", nil }))
	require.ErrorIs(t, err, ErrInvalidSchema)
}

func TestResource(t *testing.T) {
	t.Parallel()
	defs := `{"definitions":{"sku":{"type":"string","pattern":"^SKU-"}}}`
	schema, err := Compile(`{"properties":{"sku":{"$ref":"https://example.com/defs.json#/definitions/sku"}}}`,
		WithResource("https://example.com/defs.json", defs))
	require.NoError(t, err)

	_, err = schema.Validate([]byte(`{"sku":"SKU-1"}`))
	require.NoError(t, err)
	_, err = schema.Validate([]byte(`{"sku":"1"}`))
	verr, ok := errors.AsType[*SchemaValidationError](err)
	require.True(t, ok)
	assert.Equal(t, "data.sku must match pattern ^SKU-", verr.ValidationMessage)
	assert.Equal(t, "map[pattern:^SKU- type:string]", str(verr.Definition))

	// Resources are data: Validate caches the schema compiled with them.
	o := newOptions([]Option{WithResource("https://example.com/defs.json", defs)})
	_, cacheable := o.compileKey([]byte(`{}`))
	assert.True(t, cacheable)
}

func TestHTTPLoader(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/child.json":
			w.Header().Set("Content-Type", "application/schema+json")
			_, _ = w.Write([]byte(childSchema)) //nolint:errcheck // a failed write fails the test anyway
		case "/huge.json":
			_, _ = w.Write([]byte(`"` + strings.Repeat("x", MaxRemoteSchemaSize) + `"`)) //nolint:errcheck // as above
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	parent := func(path string) string { return `{"properties":{"p":{"$ref":"` + server.URL + path + `"}}}` }

	schema, err := Compile(parent("/child.json"), WithHTTPLoader(server.Client()))
	require.NoError(t, err)
	_, err = schema.Validate([]byte(`{"p":{}}`))
	require.ErrorIs(t, err, ErrSchemaValidation)

	_, err = Compile(parent("/missing.json"), WithHTTPLoader(server.Client()))
	require.ErrorIs(t, err, ErrFetch)
	require.ErrorIs(t, err, ErrInvalidSchema)

	_, err = Compile(parent("/huge.json"), WithHTTPLoader(server.Client()))
	require.ErrorIs(t, err, ErrFetch)

	// The default client works too.
	_, err = Compile(parent("/child.json"), WithHTTPLoader(nil))
	require.NoError(t, err)

	// An explicit loader wins over the HTTP one.
	before := hits.Load()
	_, err = Compile(parent("/child.json"), WithHTTPLoader(server.Client()),
		WithLoader("http", func(context.Context, string) (any, error) { return `{}`, nil }))
	require.NoError(t, err)
	assert.Equal(t, before, hits.Load())

	// Unreachable servers fail the compilation.
	_, err = Compile(`{"$ref":"http://127.0.0.1:1/x.json"}`, WithHTTPLoader(server.Client()))
	require.ErrorIs(t, err, ErrInvalidSchema)
}

func TestHTTPLoaderBadURL(t *testing.T) {
	t.Parallel()
	_, err := httpLoader(nil)(t.Context(), "http://bad host/")
	require.Error(t, err)
}
