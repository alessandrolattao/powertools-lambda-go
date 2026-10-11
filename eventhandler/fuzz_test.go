package eventhandler

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/lambdahttp"
)

func FuzzPattern(f *testing.F) {
	for _, seed := range [][2]string{
		{"GET /todos/{id}", "/todos/1"},
		{"/files/{path...}", "/files/a/b"},
		{"/a/{$}", "/a/"},
		{"POST /x%20y/{z}", "/x%20y/%2F"},
		{"/{a}/{b}", "//"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, raw, path string) {
		p, err := parsePattern(raw)
		if err != nil {
			return
		}
		decoded, err := url.PathUnescape(path)
		if err != nil {
			decoded = path
		}
		for _, trailing := range []bool{false, true} {
			values, ok := p.match(path, decoded, trailing)
			if !ok {
				continue
			}
			for name := range values {
				if !strings.Contains(raw, "{"+name) {
					t.Fatalf("value %q not named by %q", name, raw)
				}
			}
		}
		_ = p.key()
		_, _ = p.openAPIPath()
	})
}

// fuzzApp is a router with every kind of route.
func fuzzApp() *Router {
	app := New(WithCORS(CORSConfig{}), WithStripPrefixes("/api"), WithStripPrefixRegexps(regexp.MustCompile(`^/v\d+`)))
	app.Handle("GET /todos", text("list"), Compress())
	app.Handle("GET /todos/{id}", text("one"), CacheControl("max-age=60"))
	app.Handle("/files/{path...}", text("files"))
	app.HandleRegexp("", regexp.MustCompile(`/re/(?P<x>[a-z]+)/.*`), text("re"))
	Post(app, "/typed/{id}", func(_ context.Context, in struct {
		ID    int      `path:"id" minimum:"1"`
		Limit *int     `query:"limit" maximum:"10"`
		Tags  []string `query:"tag"`
		Trace string   `header:"X-Trace"`
		Body  todo
	},
	) (todo, error) {
		return in.Body, nil
	})
	return app
}

func FuzzRouter(f *testing.F) {
	f.Add("GET", "/todos", "", "", "gzip")
	f.Add("POST", "/typed/1", "limit=3&tag=a", `{"title":"abc","tags":["x"]}`, "")
	f.Add("OPTIONS", "/api/v2/files/a%2Fb", "", "", "")
	f.Add("DELETE", "/re/abc/def", "", "{", "")
	app := fuzzApp()
	f.Fuzz(func(t *testing.T, method, path, query, body, encoding string) {
		if method == "" || strings.ContainsAny(method, " \t\r\n") {
			method = http.MethodGet
		}
		req := &http.Request{
			Method: method,
			URL:    &url.URL{Path: path, RawQuery: query},
			Header: http.Header{"Accept-Encoding": {encoding}, "Origin": {"https://x"}, "Content-Type": {ContentTypeJSON}},
			Body:   io.NopCloser(strings.NewReader(body)),
		}
		inv := &lambdahttp.Invocation{TrailingSlash: len(path)%2 == 0}
		buf, err := lambdahttp.Serve(app, req.WithContext(lambdahttp.NewContext(t.Context(), inv)), inv)
		if err != nil {
			t.Fatalf("unhandled error: %v", err)
		}
		if code := buf.StatusCode(); code < 100 || code > 599 {
			t.Fatalf("status %d", code)
		}
	})
}

// FuzzAdapters feeds arbitrary events to every adapter.
func FuzzAdapters(f *testing.F) {
	f.Add(uint8(0), `{"httpMethod":"GET","path":"/todos","multiValueQueryStringParameters":{"a":["1","2"]},"isBase64Encoded":true,"body":"e30="}`)
	f.Add(uint8(1), `{"rawPath":"/dev/todos/1","rawQueryString":"x=%zz","cookies":["a=b"],"requestContext":{"stage":"dev","http":{"method":"GET","path":"/dev/todos/1"}}}`)
	f.Add(uint8(2), `{"httpMethod":"POST","path":"/typed/2","queryStringParameters":{"limit":"%41"},"multiValueHeaders":{"x-trace":["a","b"]},"body":"{}"}`)
	f.Add(uint8(3), `{"rawPath":"/files/a/b","requestContext":{"http":{"method":"PUT"}}}`)
	f.Add(uint8(4), `{"raw_path":"/todos?x=1","method":"GET","query_string_parameters":{"tag":"a,b"}}`)
	f.Add(uint8(5), `{"path":"/re/a/b","method":"GET","headers":{"h":["1",2]},"queryStringParameters":{"q":"v"}}`)
	app := fuzzApp()
	f.Fuzz(func(t *testing.T, which uint8, event string) {
		ctx := t.Context()
		var err error
		switch which % 6 {
		case 0:
			err = adapt(ctx, event, APIGatewayREST(app))
		case 1:
			err = adapt(ctx, event, APIGatewayHTTP(app))
		case 2:
			err = adapt(ctx, event, ALB(app, WithResponseSizeValidation()))
		case 3:
			err = adapt(ctx, event, FunctionURL(app))
		case 4:
			err = adapt(ctx, event, VPCLattice(app))
		default:
			err = adapt(ctx, event, VPCLatticeV2(app))
		}
		if err != nil {
			t.Fatalf("event %s: %v", event, err)
		}
	})
}

// adapt decodes the event, skipping what is not one, and runs handler.
func adapt[E, R any](ctx context.Context, event string, handler func(context.Context, E) (R, error)) error {
	var e E
	if !decodes(event, &e) {
		return nil
	}
	_, err := handler(ctx, e)
	return err
}

// decodes reports whether event decodes into e.
func decodes(event string, e any) bool {
	return json.Unmarshal([]byte(event), e) == nil
}
