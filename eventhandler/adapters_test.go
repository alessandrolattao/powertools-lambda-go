package eventhandler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// echo answers with what the request carried, and sets two cookies.
func echo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}
	cookies := map[string]string{}
	for _, c := range r.Cookies() {
		cookies[c.Name] = c.Value
	}
	http.SetCookie(w, &http.Cookie{Name: "a", Value: "1"}) //nolint:gosec // G124: the attributes do not matter here
	http.SetCookie(w, &http.Cookie{Name: "b", Value: "2"}) //nolint:gosec // G124: the attributes do not matter here
	w.Header().Add("X-Multi", "one")
	w.Header().Add("X-Multi", "two")
	if err := WriteJSON(w, r, http.StatusOK, map[string]any{
		"method": r.Method, "path": r.URL.Path, "query": r.URL.Query(), "header1": r.Header.Values("Header1"),
		"header2": r.Header.Values("Header2"), "body": string(body), "remote": r.RemoteAddr, "host": r.Host,
		"cookies": cookies, "base": BasePath(r.Context()),
	}); err != nil {
		panic(err)
	}
}

func echoApp() *Router {
	app := New()
	for _, method := range []string{"GET", "POST"} {
		app.HandleFunc(method+" /my/path", echo)
		app.HandleFunc(method+" /lambda", echo)
		app.HandleFunc(method+" /", echo)
		app.HandleFunc(method+" /testpath", echo)
		app.HandleFunc(method+" /newpath", echo)
		app.HandleFunc(method+" /todos", echo)
	}
	return app
}

func TestAPIGatewayRESTAdapter(t *testing.T) {
	t.Parallel()
	app := echoApp()
	var seen events.APIGatewayProxyRequest
	app.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		seen, _ = APIGatewayRESTEvent(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	handler := APIGatewayREST(app)

	e := loadEvent[events.APIGatewayProxyRequest](t, "apiGatewayProxyEvent.json")
	resp, err := handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, resp.IsBase64Encoded)
	assert.Nil(t, resp.Headers, "REST responses use multiValueHeaders only, as Python's serializer")
	assert.Equal(t, []string{"a=1", "b=2"}, resp.MultiValueHeaders["Set-Cookie"])
	assert.Equal(t, []string{"one", "two"}, resp.MultiValueHeaders["X-Multi"])
	assert.Equal(t, []string{ContentTypeJSON}, resp.MultiValueHeaders["Content-Type"])
	got := decode(t, resp.Body)
	assert.Equal(t, "GET", got["method"])
	assert.Equal(t, "/my/path", got["path"])
	assert.Equal(t, map[string]any{"parameter1": []any{"value1", "value2"}, "parameter2": []any{"value"}}, got["query"])
	assert.Equal(t, []any{"value1", "value2"}, got["header2"])
	assert.Equal(t, "Hello from Lambda!", got["body"])
	assert.Equal(t, "192.168.0.1/32", got["remote"])
	assert.Equal(t, "id.execute-api.us-east-1.amazonaws.com", got["host"])
	assert.Empty(t, got["base"], "the $default stage has no base path")

	// Routes ignore trailing slashes for REST APIs.
	trailing := loadEvent[events.APIGatewayProxyRequest](t, "apiGatewayProxyEventPathTrailingSlash.json")
	resp, err = handler(t.Context(), trailing)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// A stage URL has the stage as base path.
	e.RequestContext.Stage = "prod"
	e.RequestContext.Path = "/prod/my/path"
	resp, err = handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, "/prod", decode(t, resp.Body)["base"])

	e.Path, e.HTTPMethod = "/event", "GET"
	_, err = handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, "/event", seen.Path, "the raw event is in the request context")
}

func TestAPIGatewayHTTPAdapter(t *testing.T) {
	t.Parallel()
	handler := APIGatewayHTTP(echoApp())
	e := loadEvent[events.APIGatewayV2HTTPRequest](t, "apiGatewayProxyV2Event.json")
	resp, err := handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"a=1", "b=2"}, resp.Cookies)
	assert.Equal(t, "one, two", resp.Headers["X-Multi"])
	assert.NotContains(t, resp.Headers, "Set-Cookie")
	got := decode(t, resp.Body)
	assert.Equal(t, "POST", got["method"])
	assert.Equal(t, map[string]any{"parameter1": []any{"value1", "value2"}, "parameter2": []any{"value"}}, got["query"],
		"the raw query string keeps repeated parameters apart")
	assert.JSONEq(t, `{"message": "hello world", "username": "tom"}`, str(t, got["body"]))
	assert.Equal(t, "192.168.0.1/32", got["remote"])
	assert.Equal(t, []any{"value1,value2"}, got["header2"])

	e.Cookies = []string{"session=abc", "theme=dark"}
	e.RequestContext.Stage = "dev"
	e.RawPath = "/dev/my/path"
	e.RequestContext.HTTP.Path = "/dev/my/path"
	resp, err = handler(t.Context(), e)
	require.NoError(t, err)
	got = decode(t, resp.Body)
	assert.Equal(t, map[string]any{"session": "abc", "theme": "dark"}, got["cookies"])
	assert.Equal(t, "/my/path", got["path"], "the stage is stripped from the raw path")
	assert.Equal(t, "/dev", got["base"])

	trailing := loadEvent[events.APIGatewayV2HTTPRequest](t, "apiGatewayProxyV2EventPathTrailingSlash.json")
	resp, err = handler(t.Context(), trailing)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no trailing slash tolerance, as in Python")
	assert.JSONEq(t, `{"statusCode":404,"message":"Not found"}`, resp.Body)
	assert.Equal(t, "application/json", resp.Headers["Content-Type"])
}

func TestFunctionURLAdapter(t *testing.T) {
	t.Parallel()
	handler := FunctionURL(echoApp())
	e := loadEvent[events.LambdaFunctionURLRequest](t, "lambdaFunctionUrlEventWithHeaders.json")
	resp, err := handler(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"a=1", "b=2"}, resp.Cookies)
	got := decode(t, resp.Body)
	assert.Equal(t, "GET", got["method"])
	assert.Equal(t, "/", got["path"])
	assert.Equal(t, "123.123.123.123", got["remote"])
	assert.Equal(t, "<url-id>.lambda-url.us-east-1.on.aws", got["host"])

	plain := loadEvent[events.LambdaFunctionURLRequest](t, "lambdaFunctionUrlEvent.json")
	_, err = handler(t.Context(), plain)
	require.NoError(t, err)

	trailing := loadEvent[events.LambdaFunctionURLRequest](t, "lambdaFunctionUrlEventPathTrailingSlash.json")
	resp, err = handler(t.Context(), trailing)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestALBAdapter(t *testing.T) {
	t.Parallel()
	handler := ALB(echoApp())

	single := loadEvent[events.ALBTargetGroupRequest](t, "albEvent.json")
	resp, err := handler(t.Context(), single)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "200 OK", resp.StatusDescription)
	assert.Nil(t, resp.MultiValueHeaders)
	assert.Equal(t, "b=2", resp.Headers["Set-Cookie"], "single-value mode keeps the last cookie")
	assert.Equal(t, "two", resp.Headers["X-Multi"])
	got := decode(t, resp.Body)
	assert.Equal(t, "/lambda", got["path"])
	assert.Equal(t, map[string]any{"query": []any{"1234ABCD"}}, got["query"])
	assert.Equal(t, "Test", got["body"])
	assert.Equal(t, "72.12.164.125", got["remote"])

	multi := loadEvent[events.ALBTargetGroupRequest](t, "albMultiValueHeadersEvent.json")
	resp, err = handler(t.Context(), multi)
	require.NoError(t, err)
	assert.Equal(t, []string{"a=1", "b=2"}, resp.MultiValueHeaders["Set-Cookie"])
	assert.Nil(t, resp.Headers)

	query := loadEvent[events.ALBTargetGroupRequest](t, "albMultiValueQueryStringEvent.json")
	resp, err = handler(t.Context(), query)
	require.NoError(t, err)
	got = decode(t, resp.Body)
	assert.Equal(t, map[string]any{"parameter1": []any{"value1", "value2"}, "parameter2": []any{"value"}}, got["query"])
	assert.Equal(t, []any{"value1", "value2"}, got["header2"])

	// ALB passes query strings percent-encoded.
	single.QueryStringParameters = map[string]string{"q": "a%20b%26c"}
	resp, err = handler(t.Context(), single)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"q": []any{"a b&c"}}, decode(t, resp.Body)["query"])

	trailing := loadEvent[events.ALBTargetGroupRequest](t, "albEventPathTrailingSlash.json")
	resp, err = handler(t.Context(), trailing)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestVPCLatticeAdapters(t *testing.T) {
	t.Parallel()
	app := echoApp()
	Get(app, "/list", func(_ context.Context, in struct {
		IDs []int `query:"id"`
	},
	) ([]int, error) {
		return in.IDs, nil
	})

	v1 := VPCLattice(app)
	e := loadEvent[eventsource.VPCLatticeEvent](t, "vpcLatticeEvent.json")
	resp, err := v1(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "200 OK", resp.StatusDescription)
	assert.Equal(t, "b=2", resp.Headers["Set-Cookie"])
	assert.Equal(t, "one, two", resp.Headers["X-Multi"])
	got := decode(t, resp.Body)
	assert.Equal(t, "/testpath", got["path"])
	assert.JSONEq(t, `{"test": "event"}`, str(t, got["body"]), "the base64 body is decoded")
	assert.Equal(t, map[string]any{"order-id": []any{"1"}}, got["query"])
	assert.Equal(t, "10.213.229.10", got["remote"])

	// Repeated parameters arrive folded with commas in version 1.
	e.RawPath, e.Method, e.Body = "/list", "GET", ""
	e.QueryStringParameters = map[string]string{"id": "1,2,3"}
	resp, err = v1(t.Context(), e)
	require.NoError(t, err)
	assert.JSONEq(t, "[1,2,3]", resp.Body)

	trailing := loadEvent[eventsource.VPCLatticeEvent](t, "vpcLatticeEventPathTrailingSlash.json")
	resp, err = v1(t.Context(), trailing)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	v2 := VPCLatticeV2(app)
	e2 := loadEvent[eventsource.VPCLatticeEventV2](t, "vpcLatticeV2EventWithHeaders.json")
	resp, err = v2(t.Context(), e2)
	require.NoError(t, err)
	got = decode(t, resp.Body)
	assert.Equal(t, "/newpath", got["path"])
	assert.Equal(t, []any{"value1", "value2"}, got["header2"])
	assert.Equal(t, map[string]any{"parameter1": []any{"value1", "value2"}, "parameter2": []any{"value"}}, got["query"])
	assert.Equal(t, "172.31.40.143", got["remote"])

	plain := loadEvent[eventsource.VPCLatticeEventV2](t, "vpcLatticeV2Event.json")
	resp, err = v2(t.Context(), plain)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	trailing2 := loadEvent[eventsource.VPCLatticeEventV2](t, "vpcLatticeEventV2PathTrailingSlash.json")
	resp, err = v2(t.Context(), trailing2)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// A pre-flight body is empty, never null.
	cors := New(WithCORS(CORSConfig{}))
	resp, err = VPCLatticeV2(cors)(t.Context(), eventsource.VPCLatticeEventV2{Path: "/my/request", Method: "OPTIONS"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, resp.Body)
}

func TestBinaryBodies(t *testing.T) {
	t.Parallel()
	png := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xff}
	app := New()
	app.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", "image/png")
		if _, err := w.Write(data); err != nil {
			panic(err)
		}
	})
	app.Handle("GET /text", text(strings.Repeat("hello ", 50)), Compress())

	e := events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Path: "/upload", IsBase64Encoded: true,
		Body: base64.StdEncoding.EncodeToString(png),
	}
	resp, err := APIGatewayREST(app)(t.Context(), e)
	require.NoError(t, err)
	assert.True(t, resp.IsBase64Encoded)
	decoded, err := base64.StdEncoding.DecodeString(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, png, decoded)

	gz := events.APIGatewayV2HTTPRequest{
		RawPath: "/text", Headers: map[string]string{"accept-encoding": "gzip"},
		RequestContext: events.APIGatewayV2HTTPRequestContext{HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: "GET"}},
	}
	resp2, err := APIGatewayHTTP(app)(t.Context(), gz)
	require.NoError(t, err)
	assert.True(t, resp2.IsBase64Encoded, "compressed bodies are binary")
	assert.Equal(t, "gzip", resp2.Headers["Content-Encoding"])
	raw, err := base64.StdEncoding.DecodeString(resp2.Body)
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	plain, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("hello ", 50), string(plain))

	resp, err = APIGatewayREST(app)(t.Context(), events.APIGatewayProxyRequest{HTTPMethod: "POST", Path: "/upload", IsBase64Encoded: true, Body: "!!"})
	require.NoError(t, err)
	decoded, err = base64.StdEncoding.DecodeString(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "!!", string(decoded), "a body flagged base64 that is not passes as it is")
}

func TestIsText(t *testing.T) {
	t.Parallel()
	for ct, want := range map[string]bool{
		"":                                  true,
		"text/html; charset=utf-8":          true,
		"application/json":                  true,
		"application/problem+json":          true,
		"application/atom+xml":              true,
		"application/x-www-form-urlencoded": true,
		"image/png":                         false,
		"application/octet-stream":          false,
		"application/pdf":                   false,
		";;":                                false,
	} {
		assert.Equal(t, want, isText(ct), ct)
	}
}

func TestUnhandledErrorFailsTheInvocation(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	app := New(WithDebug(false))
	Get(app, "/x", func(context.Context, struct{}) (string, error) { return "", boom })
	_, err := APIGatewayREST(app)(t.Context(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/x"})
	require.ErrorIs(t, err, boom)

	dbg := New(WithDebug(true))
	Get(dbg, "/x", func(context.Context, struct{}) (string, error) { return "", boom })
	resp, err := APIGatewayREST(dbg)(t.Context(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/x"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, resp.Body, "boom")
}

func TestALBResponseSizeValidation(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", ALBMaxResponseSize)
	app := New()
	app.Handle("GET /big", text(big))
	app.Handle("GET /small", text("small"))
	e := events.ALBTargetGroupRequest{HTTPMethod: "GET", Path: "/big"}

	resp, err := ALB(app)(t.Context(), e)
	require.NoError(t, err, "no validation unless asked")
	assert.Len(t, resp.Body, ALBMaxResponseSize)

	validated := ALB(app, WithResponseSizeValidation())
	_, err = validated(t.Context(), e)
	var sizeErr *ResponseSizeExceededError
	require.ErrorAs(t, err, &sizeErr)
	assert.Equal(t, ALBMaxResponseSize, sizeErr.MaxSize)
	assert.Greater(t, sizeErr.ActualSize, ALBMaxResponseSize)
	assert.Contains(t, sizeErr.Error(), "exceeds the maximum allowed size (1048576 bytes)")

	resp, err = validated(t.Context(), events.ALBTargetGroupRequest{HTTPMethod: "GET", Path: "/small"})
	require.NoError(t, err)
	assert.Equal(t, "small", resp.Body)

	OnError(app, func(w http.ResponseWriter, r *http.Request, _ *ResponseSizeExceededError) error {
		return WriteJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "too big"})
	})
	resp, err = validated(t.Context(), e)
	require.NoError(t, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.JSONEq(t, `{"error":"too big"}`, resp.Body)

	stillBig := New()
	stillBig.Handle("GET /big", text(big))
	OnError(stillBig, func(w http.ResponseWriter, _ *http.Request, _ *ResponseSizeExceededError) error {
		write(w, big)
		return nil
	})
	_, err = ALB(stillBig, WithResponseSizeValidation())(t.Context(), e)
	require.ErrorAs(t, err, &sizeErr, "the handled response is checked again")

	plain := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { write(w, big) })
	_, err = ALB(plain, WithResponseSizeValidation())(t.Context(), e)
	require.ErrorAs(t, err, &sizeErr, "without a router nobody can handle the error")
}

func TestEventGetters(t *testing.T) {
	t.Parallel()
	app := New()
	var sources []string
	app.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if _, ok := APIGatewayRESTEvent(ctx); ok {
			sources = append(sources, "rest")
		}
		if _, ok := APIGatewayHTTPEvent(ctx); ok {
			sources = append(sources, "http")
		}
		if _, ok := ALBEvent(ctx); ok {
			sources = append(sources, "alb")
		}
		if _, ok := FunctionURLEvent(ctx); ok {
			sources = append(sources, "url")
		}
		if _, ok := VPCLatticeEvent(ctx); ok {
			sources = append(sources, "lattice")
		}
		if _, ok := VPCLatticeV2Event(ctx); ok {
			sources = append(sources, "lattice2")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ctx := t.Context()
	_, err := APIGatewayREST(app)(ctx, events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/"})
	require.NoError(t, err)
	_, err = APIGatewayHTTP(app)(ctx, events.APIGatewayV2HTTPRequest{RawPath: "/"})
	require.NoError(t, err)
	_, err = ALB(app)(ctx, events.ALBTargetGroupRequest{HTTPMethod: "GET", Path: "/"})
	require.NoError(t, err)
	_, err = FunctionURL(app)(ctx, events.LambdaFunctionURLRequest{RawPath: "/"})
	require.NoError(t, err)
	_, err = VPCLattice(app)(ctx, eventsource.VPCLatticeEvent{RawPath: "/", Method: "GET"})
	require.NoError(t, err)
	_, err = VPCLatticeV2(app)(ctx, eventsource.VPCLatticeEventV2{Path: "/", Method: "GET"})
	require.NoError(t, err)
	assert.Equal(t, []string{"rest", "http", "alb", "url", "lattice", "lattice2"}, sources)

	_, ok := APIGatewayRESTEvent(ctx)
	assert.False(t, ok)
	assert.Empty(t, BasePath(ctx))
	_, ok = RequestFromContext(ctx)
	assert.False(t, ok)
}
