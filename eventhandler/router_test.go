package eventhandler

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoutingSemantics(t *testing.T) {
	t.Parallel()
	app := New()
	app.Handle("GET /todos/{id}", text("dynamic"))
	app.Handle("GET /todos/fetch", text("static"))
	app.Handle("GET /files/{path...}", text("files"))
	app.Handle("GET /a/{x}", text("first"))
	app.Handle("GET /{y}/b", text("second"))
	app.Handle("POST /todos", text("create"))
	app.Handle("/any", text("any method"))
	app.HandleRegexp(http.MethodGet, regexp.MustCompile(`/v\d+/status`), text("regexp"))
	app.HandleRegexp(http.MethodGet, regexp.MustCompile(`/users/(?P<user>\w+)`), text("user"))

	tests := []struct {
		name, method, path string
		status             int
		body               string
	}{
		{"static before dynamic", "GET", "/todos/fetch", 200, "static"},
		{"dynamic", "GET", "/todos/1", 200, "dynamic"},
		{"registration order among dynamic routes", "GET", "/a/b", 200, "first"},
		{"catch-all", "GET", "/files/x/y.txt", 200, "files"},
		{"method mismatch is a 404", "DELETE", "/todos/1", 404, `{"statusCode":404,"message":"Not found"}`},
		{"no HEAD from GET", "HEAD", "/todos/fetch", 404, `{"statusCode":404,"message":"Not found"}`},
		{"no redirect for unclean paths", "GET", "/todos//fetch", 404, `{"statusCode":404,"message":"Not found"}`},
		{"no trailing slash tolerance outside REST", "GET", "/todos/fetch/", 404, `{"statusCode":404,"message":"Not found"}`},
		{"pattern without method", "PATCH", "/any", 200, "any method"},
		{"regular expression without groups is static", "GET", "/v2/status", 200, "regexp"},
		{"regular expression with groups", "GET", "/users/bob", 200, "user"},
		{"regular expressions match the whole path", "GET", "/users/bob/x", 404, `{"statusCode":404,"message":"Not found"}`},
	}
	for _, tc := range tests {
		rec := do(t, app, tc.method, tc.path, "")
		assert.Equal(t, tc.status, rec.Code, tc.name)
		if strings.HasPrefix(tc.body, "{") {
			assert.JSONEq(t, tc.body, rec.Body.String(), tc.name)
		} else {
			assert.Equal(t, tc.body, rec.Body.String(), tc.name)
		}
	}
}

func TestPathValuesAndPattern(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleFunc("GET /users/{user}/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		write(w, r.PathValue("user")+"|"+r.PathValue("path")+"|"+r.Pattern)
	})
	app.HandleRegexp(http.MethodGet, regexp.MustCompile(`/re/(?P<id>\d+)`), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, r.PathValue("id"))
	}))
	rec := do(t, app, "GET", "/users/b%20ob/files/a/b%2Fc", "")
	assert.Equal(t, "b ob|a/b/c|GET /users/{user}/files/{path...}", rec.Body.String())
	assert.Equal(t, "42", do(t, app, "GET", "/re/42", "").Body.String())
}

func TestDuplicateRoutesPanic(t *testing.T) {
	t.Parallel()
	app := New()
	app.Handle("GET /todos/{id}", text("x"))
	assert.PanicsWithError(t, `duplicate route: "GET /todos/{other}" conflicts with a route registered before`, func() {
		app.Handle("GET /todos/{other}", text("y"))
	})
	assert.NotPanics(t, func() { app.Handle("POST /todos/{id}", text("z")) })
	assert.Panics(t, func() { app.Handle("bad pattern", text("z")) })
}

func TestStripPrefixes(t *testing.T) {
	t.Parallel()
	app := New(
		WithStripPrefixes("/payment"),
		WithStripPrefixRegexps(regexp.MustCompile(`^/v[123]+/(dev|stg)`)),
	)
	app.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { write(w, "root "+r.URL.Path) })
	app.HandleFunc("GET /todos", func(w http.ResponseWriter, r *http.Request) { write(w, "todos "+r.URL.Path) })

	tests := map[string]string{
		"/payment":       "root /",
		"/payment/todos": "todos /todos",
		"/v1/dev/todos":  "todos /todos",
		"/v2/stg":        "root /",
		"/todos":         "todos /todos",
	}
	for path, want := range tests {
		assert.Equal(t, want, do(t, app, "GET", path, "").Body.String(), path)
	}
	assert.Equal(t, http.StatusNotFound, do(t, app, "GET", "/paymentx/todos", "").Code)
}

// mark is a middleware that records its passage in the X-Trail header.
func mark(name string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-Trail", name+":before")
			next.ServeHTTP(w, r)
			w.Header().Add("X-Trail", name+":after")
		})
	}
}

func TestMiddlewareOrder(t *testing.T) {
	t.Parallel()
	app := New()
	app.Use(mark("global1"), mark("global2"))
	app.Handle("GET /x", text("ok"), Middlewares(mark("route1"), mark("route2")))

	rec := do(t, app, "GET", "/x", "")
	assert.Equal(t, []string{
		"global1:before", "global2:before", "route1:before", "route2:before",
		"route2:after", "route1:after", "global2:after", "global1:after",
	}, rec.Header().Values("X-Trail"))

	notFound := do(t, app, "GET", "/missing", "")
	assert.Equal(t, http.StatusNotFound, notFound.Code)
	assert.Equal(t, []string{"global1:before", "global2:before", "global2:after", "global1:after"},
		notFound.Header().Values("X-Trail"), "global middlewares run for not found requests too")
}

func TestMiddlewareShortCircuit(t *testing.T) {
	t.Parallel()
	app := New()
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				Error(w, r, Unauthorized("Unauthorized"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	called := false
	app.Use(mark("outer"), auth)
	app.HandleFunc("GET /secret", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		write(w, "secret")
	})

	rec := do(t, app, "GET", "/secret", "")
	assert.False(t, called)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"statusCode":401,"message":"Unauthorized"}`, rec.Body.String())
	assert.Equal(t, []string{"outer:before", "outer:after"}, rec.Header().Values("X-Trail"),
		"outer middlewares see the short-circuited response")

	assert.Equal(t, "secret", do(t, app, "GET", "/secret", "", "Authorization", "x").Body.String())
}

func TestIncludeRouter(t *testing.T) {
	t.Parallel()
	todos := New()
	todos.Use(mark("router"))
	todos.Handle("GET /", text("list"))
	todos.Handle("GET /{id}", text("one"))
	OnError(todos, func(w http.ResponseWriter, r *http.Request, _ *teapotError) error {
		return WriteJSON(w, r, http.StatusTeapot, map[string]string{"from": "router"})
	})
	todos.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) { Error(w, r, &teapotError{}) })

	app := New()
	app.Use(mark("app"))
	app.Include(todos, "/todos")

	rec := do(t, app, "GET", "/todos", "")
	assert.Equal(t, "list", rec.Body.String())
	assert.Equal(t, []string{"app:before", "router:before", "router:after", "app:after"}, rec.Header().Values("X-Trail"))
	assert.Equal(t, "one", do(t, app, "GET", "/todos/7", "").Body.String())
	assert.Equal(t, http.StatusTeapot, do(t, app, "GET", "/todos/fail", "").Code, "error handlers merge")
	assert.Equal(t, http.StatusNotFound, do(t, app, "GET", "/", "").Code)
}

type teapotError struct{ wrapped error }

func (e *teapotError) Error() string { return "teapot" }
func (e *teapotError) Unwrap() error { return e.wrapped }

type timeoutError interface {
	error
	Timeout() bool
}

type slowError struct{}

func (slowError) Error() string { return "slow" }
func (slowError) Timeout() bool { return true }

func TestErrorHandlerPrecedence(t *testing.T) {
	t.Parallel()
	app := New()
	answer := func(name string) func(w http.ResponseWriter, r *http.Request) error {
		return func(w http.ResponseWriter, r *http.Request) error {
			return WriteJSON(w, r, http.StatusTeapot, map[string]string{"handler": name})
		}
	}
	OnError(app, func(w http.ResponseWriter, r *http.Request, _ error) error { return answer("catch-all")(w, r) })
	OnError(app, func(w http.ResponseWriter, r *http.Request, _ timeoutError) error { return answer("timeout")(w, r) })
	OnError(app, func(w http.ResponseWriter, r *http.Request, _ *teapotError) error { return answer("teapot")(w, r) })
	OnError(app, func(w http.ResponseWriter, r *http.Request, _ *ServiceError) error { return answer("service")(w, r) })

	cases := map[string]error{
		"teapot":    &teapotError{},
		"service":   fmt.Errorf("wrapped: %w", BadRequest("x")),
		"timeout":   fmt.Errorf("wrapped: %w", slowError{}),
		"catch-all": errors.New("anything"),
	}
	// The outermost error with a handler for its concrete type wins over
	// the errors it wraps and over interface handlers.
	cases["teapot"] = &teapotError{wrapped: BadRequest("inner")}
	for want, err := range cases {
		app.HandleFunc("GET /"+want, func(w http.ResponseWriter, r *http.Request) { Error(w, r, err) })
	}
	for want := range cases {
		rec := do(t, app, "GET", "/"+want, "")
		assert.Equal(t, want, decode(t, rec.Body.String())["handler"], want)
	}
	// A 404 goes to the not found handler first.
	app.HandleNotFound(text("custom not found"))
	assert.Equal(t, "custom not found", do(t, app, "GET", "/missing", "").Body.String())
}

func TestErrorHandlerReturningError(t *testing.T) {
	t.Parallel()
	app := New()
	OnError(app, func(_ http.ResponseWriter, _ *http.Request, err *teapotError) error {
		return Forbidden("converted " + err.Error())
	})
	app.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) {
		write(w, "partial")
		Error(w, r, &teapotError{})
	})
	rec := do(t, app, "GET", "/x", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"statusCode":403,"message":"converted teapot"}`, rec.Body.String())
}

func TestUnhandledErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	local := New(WithDebug(false))
	local.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) { Error(w, r, boom) })
	rec := do(t, local, "GET", "/x", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "a local server answers 500")
	assert.JSONEq(t, `{"statusCode":500,"message":"Internal Server Error"}`, rec.Body.String())

	dbg := New(WithDebug(true))
	dbg.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) { Error(w, r, boom) })
	rec = do(t, dbg, "GET", "/x", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, ContentTypeText, rec.Header().Get("Content-Type"))
	assert.True(t, strings.HasPrefix(rec.Body.String(), "boom\n\n"))
	assert.Contains(t, rec.Body.String(), "goroutine")
}

func TestDebugFromEnvironment(t *testing.T) {
	t.Setenv("POWERTOOLS_DEV", "true")
	assert.True(t, New().debug)
	assert.False(t, New(WithDebug(false)).debug, "the option wins over the variable")
	t.Setenv("POWERTOOLS_DEV", "false")
	assert.False(t, New().debug)
}

func TestServiceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err    *ServiceError
		status int
		msg    string
	}{
		{BadRequest("bad"), 400, "bad"},
		{Unauthorized("who"), 401, "who"},
		{Forbidden("no"), 403, "no"},
		{NotFound(""), 404, "Not found"},
		{RequestTimeout("slow"), 408, "slow"},
		{RequestEntityTooLarge("big"), 413, "big"},
		{InternalServerError("oops"), 500, "oops"},
		{ServiceUnavailable("later"), 503, "later"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.status, tc.err.StatusCode)
		assert.Equal(t, tc.msg, tc.err.Error())
	}
	require.ErrorIs(t, fmt.Errorf("x: %w", NotFound("todo 1")), ErrNotFound)
	require.NotErrorIs(t, NotFound("x"), ErrBadRequest)
	assert.JSONEq(t, `{"field":"x"}`, NewServiceError(400, map[string]string{"field": "x"}).Error())

	app := New()
	app.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, NewServiceError(http.StatusConflict, map[string]any{"code": 7}))
	})
	assert.JSONEq(t, `{"statusCode":409,"message":{"code":7}}`, do(t, app, "GET", "/x", "").Body.String())
}

func TestErrorOutsideRouter(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { Error(w, r, NotFound("gone")) })
	rec := do(t, h, "GET", "/", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"statusCode":404,"message":"gone"}`, rec.Body.String())

	h = func(w http.ResponseWriter, r *http.Request) { Error(w, r, errors.New("x")) }
	assert.Equal(t, http.StatusInternalServerError, do(t, h, "GET", "/", "").Code)
}

func TestCORS(t *testing.T) {
	t.Parallel()
	app := New(WithCORS(CORSConfig{
		AllowOrigin:      "https://www.example.com",
		ExtraOrigins:     []string{"https://dev.example.com"},
		AllowHeaders:     []string{"X-Custom"},
		ExposeHeaders:    []string{"X-Exposed", "X-Other"},
		MaxAge:           new(100),
		AllowCredentials: true,
	}))
	app.Handle("GET /todos", text("ok"))
	app.Handle("POST /todos", text("ok"))
	app.Handle("DELETE /todos", text("ok"), CORS(false))

	rec := do(t, app, "GET", "/todos", "", "Origin", "https://dev.example.com")
	h := rec.Header()
	assert.Equal(t, "https://dev.example.com", h.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Authorization,Content-Type,X-Amz-Date,X-Amz-Security-Token,X-Api-Key,X-Custom", h.Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "X-Exposed,X-Other", h.Get("Access-Control-Expose-Headers"))
	assert.Equal(t, "100", h.Get("Access-Control-Max-Age"))
	assert.Equal(t, "true", h.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "Origin", h.Get("Vary"))
	assert.Empty(t, h.Get("Access-Control-Allow-Methods"), "methods only on pre-flight")

	assert.Empty(t, do(t, app, "GET", "/todos", "", "Origin", "https://evil.example").Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, do(t, app, "GET", "/todos", "").Header().Get("Access-Control-Allow-Origin"), "no Origin, no CORS")
	assert.Empty(t, do(t, app, "DELETE", "/todos", "", "Origin", "https://www.example.com").Header().Get("Access-Control-Allow-Origin"))

	pre := do(t, app, "OPTIONS", "/todos", "", "Origin", "https://www.example.com")
	assert.Equal(t, http.StatusNoContent, pre.Code)
	assert.Empty(t, pre.Body.String())
	assert.Empty(t, pre.Header().Get("Content-Type"))
	assert.Equal(t, "GET,OPTIONS,POST", pre.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "https://www.example.com", pre.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSWildcard(t *testing.T) {
	t.Parallel()
	app := New(WithCORS(CORSConfig{AllowCredentials: true}))
	app.Handle("GET /x", text("ok"))
	h := do(t, app, "GET", "/x", "", "Origin", "https://any.example").Header()
	assert.Equal(t, "*", h.Get("Access-Control-Allow-Origin"))
	assert.Empty(t, h.Get("Access-Control-Allow-Credentials"), "never with a wildcard origin")
	assert.Empty(t, h.Get("Vary"))

	// A route can turn CORS on in a router without it.
	plain := New()
	plain.Handle("GET /x", text("ok"), CORS(true))
	assert.Equal(t, "*", do(t, plain, "GET", "/x", "", "Origin", "https://a").Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, http.StatusNotFound, do(t, plain, "OPTIONS", "/x", "").Code, "no pre-flight without WithCORS")
}

func TestCompression(t *testing.T) {
	t.Parallel()
	app := New()
	long := strings.Repeat("compress me ", 100)
	app.Handle("GET /gzip", text(long), Compress())
	app.Handle("GET /plain", text(long))
	app.Handle("GET /empty", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), Compress())

	rec := do(t, app, "GET", "/gzip", "", "Accept-Encoding", "deflate, gzip;q=1.0")
	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, long, string(plain))

	assert.Empty(t, do(t, app, "GET", "/gzip", "").Header().Get("Content-Encoding"), "the client must accept gzip")
	assert.Empty(t, do(t, app, "GET", "/plain", "", "Accept-Encoding", "gzip").Header().Get("Content-Encoding"))
	assert.Empty(t, do(t, app, "GET", "/empty", "", "Accept-Encoding", "gzip").Header().Get("Content-Encoding"))
}

func TestCacheControl(t *testing.T) {
	t.Parallel()
	app := New()
	app.Handle("GET /ok", text("x"), CacheControl("max-age=3600"))
	app.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) { Error(w, r, BadRequest("x")) }, CacheControl("max-age=3600"))
	assert.Equal(t, "max-age=3600", do(t, app, "GET", "/ok", "").Header().Get("Cache-Control"))
	assert.Equal(t, "no-cache", do(t, app, "GET", "/fail", "").Header().Get("Cache-Control"))
}

func TestContentTypeSniffing(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleFunc("GET /html", func(w http.ResponseWriter, _ *http.Request) { write(w, "<html><body>hi</body></html>") })
	assert.Equal(t, "text/html; charset=utf-8", do(t, app, "GET", "/html", "").Header().Get("Content-Type"))
}
