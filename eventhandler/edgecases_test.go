package eventhandler

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/swaggest/jsonschema-go"
)

func TestKeywordWildcardNames(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleFunc("GET /items/{type}/{range}", func(w http.ResponseWriter, r *http.Request) {
		write(w, r.PathValue("type")+r.PathValue("range"))
	})
	assert.Equal(t, "ab", do(t, app, "GET", "/items/a/b", "").Body.String())
	_, err := parsePattern("/{1a}")
	require.ErrorIs(t, err, ErrInvalidPattern)
}

func TestIncludeRegexpAlternation(t *testing.T) {
	t.Parallel()
	sub := New()
	sub.HandleRegexp(http.MethodGet, regexp.MustCompile(`/a|/b`), text("ab"))
	app := New()
	app.Include(sub, "/api")
	assert.Equal(t, http.StatusOK, do(t, app, "GET", "/api/b", "").Code)
	assert.Equal(t, http.StatusNotFound, do(t, app, "GET", "/b", "").Code, "the prefix applies to every branch")
}

func TestVaryHeaders(t *testing.T) {
	t.Parallel()
	app := New(WithCORS(CORSConfig{AllowOrigin: "https://app.example.com"}))
	app.Handle("GET /x", text(strings.Repeat("x", 100)), Compress())
	for _, origin := range []string{"", "https://evil.example", "https://app.example.com"} {
		var header []string
		if origin != "" {
			header = []string{"Origin", origin}
		}
		vary := do(t, app, "GET", "/x", "", header...).Header().Values("Vary")
		assert.Equal(t, []string{"Origin", "Accept-Encoding"}, vary, origin)
	}
	wildcard := New(WithCORS(CORSConfig{}))
	wildcard.Handle("GET /x", text("x"))
	assert.Empty(t, do(t, wildcard, "GET", "/x", "", "Origin", "https://a").Header().Values("Vary"))
}

func TestErrorHandlersDoNotRecurse(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleNotFound(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, NotFound("still missing"))
	}))
	OnError(app, func(w http.ResponseWriter, r *http.Request, err *teapotError) error {
		Error(w, r, err)
		return nil
	})
	app.HandleFunc("GET /teapot", func(w http.ResponseWriter, r *http.Request) { Error(w, r, &teapotError{}) })

	rec := do(t, app, "GET", "/missing", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"statusCode":404,"message":"still missing"}`, rec.Body.String())
	assert.Equal(t, http.StatusInternalServerError, do(t, app, "GET", "/teapot", "").Code)
}

func TestContentTypes(t *testing.T) {
	t.Parallel()
	app := New()
	Post(app, "/x", func(context.Context, createTodo) (bool, error) { return true, nil })
	Put(app, "/x", func(context.Context, optionalBody) (bool, error) { return true, nil })
	body := `{"title":"abc"}`
	for ct, status := range map[string]int{
		"Application/JSON; charset=utf-8": http.StatusOK,
		"application/problem+json":        http.StatusOK,
		"application/jsonfoo":             http.StatusUnsupportedMediaType,
		"text/plain":                      http.StatusUnsupportedMediaType,
	} {
		assert.Equal(t, status, do(t, app, "POST", "/x", body, "Content-Type", ct).Code, ct)
	}
	assert.Equal(t, http.StatusOK, do(t, app, "PUT", "/x", "", "Content-Type", "text/plain").Code, "an empty optional body has no type to check")
	rec := do(t, app, "POST", "/x", `{"title":"x","title":"abcd"}`)
	assert.Equal(t, http.StatusOK, rec.Code, "duplicate names: the last one wins, as in Python")
}

func TestResponseCookiesAddToMiddlewareCookies(t *testing.T) {
	t.Parallel()
	app := New()
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Set-Cookie", "trace=1")
			next.ServeHTTP(w, r)
		})
	})
	Get(app, "/x", func(context.Context, struct{}) (Response[string], error) {
		return Response[string]{Header: http.Header{"Set-Cookie": {"session=2"}}, Body: "ok"}, nil
	})
	assert.Equal(t, []string{"trace=1", "session=2"}, do(t, app, "GET", "/x", "").Header().Values("Set-Cookie"))
}

func TestALBSizeErrorHandlerSeesThePathValues(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleFunc("GET /big/{id}", func(w http.ResponseWriter, _ *http.Request) { write(w, strings.Repeat("x", ALBMaxResponseSize)) })
	OnError(app, func(w http.ResponseWriter, r *http.Request, _ *ResponseSizeExceededError) error {
		return WriteJSON(w, r, http.StatusOK, r.PathValue("id")+" "+r.Pattern)
	})
	resp, err := ALB(app, WithResponseSizeValidation())(t.Context(), events.ALBTargetGroupRequest{HTTPMethod: "GET", Path: "/big/7"})
	require.NoError(t, err)
	assert.Equal(t, `"7 GET /big/{id}"`, resp.Body)
}

// notThis shapes a schema with a keyword the validator does not enforce.
type notThis struct {
	A string `json:"a"`
}

func (notThis) PrepareJSONSchema(s *jsonschema.Schema) error {
	s.WithNot((&jsonschema.Schema{}).ToSchemaOrBool())
	return nil
}

func TestUnsupportedSchemaFailsAtRegistration(t *testing.T) {
	t.Parallel()
	defer func() {
		err, ok := recover().(error)
		require.True(t, ok, "Register panics with an error")
		require.ErrorIs(t, err, ErrInvalidOperation)
		require.ErrorContains(t, err, `keyword "not"`)
	}()
	Post(New(), "/x", func(context.Context, struct{ Body notThis }) (bool, error) { return true, nil })
}
