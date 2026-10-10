package tracing

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	smithytracing "github.com/aws/smithy-go/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestAWSTracerProviderNestsSDKSpans(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)

	ctx, parent := tracer.Provider().Tracer("test").Start(t.Context(), "handler")
	_, call := tracer.AWSTracerProvider().Tracer("aws").StartSpan(ctx, "DynamoDB.GetItem",
		func(o *smithytracing.SpanOptions) { o.Kind = smithytracing.SpanKindClient })
	call.End()
	parent.End()

	span := spanNamed(t, recorder, "DynamoDB.GetItem")
	assert.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
}

func TestAWSTracerProviderOfADisabledTracer(t *testing.T) {
	t.Parallel()
	assert.IsType(t, smithytracing.NopTracerProvider{}, New(WithDisabled(true)).AWSTracerProvider())
}

// server answers every request, remembering the X-Ray header it carried.
func server(t *testing.T) (srv *httptest.Server, headers func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Amzn-Trace-Id"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func get(t *testing.T, client *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func TestHTTPClientTracesRequests(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	srv, headers := server(t)

	ctx, parent := tracer.Provider().Tracer("test").Start(t.Context(), "handler")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/orders", http.NoBody)
	require.NoError(t, err)
	resp, err := tracer.HTTPClient().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	parent.End()

	call := spanNamed(t, recorder, "HTTP GET")
	assert.Equal(t, trace.SpanKindClient, call.SpanKind())
	assert.Equal(t, parent.SpanContext().SpanID(), call.Parent().SpanID())
	require.Len(t, headers(), 1)
	assert.Contains(t, headers()[0], "Root=1-", "the X-Ray header goes downstream")
	assert.Contains(t, headers()[0], call.SpanContext().SpanID().String())
}

func TestIgnoreEndpoint(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	srv, _ := server(t)

	client := tracer.HTTPClient(
		IgnoreEndpoint("", "/health"),
		IgnoreEndpoint("127.0.0.*", "/metrics", "/metrics?format=json"),
		IgnoreEndpoint("*.example.com"),
		IgnoreEndpoint(""),
	)
	for _, path := range []string{"/health", "/metrics", "/metrics?format=json", "/orders", "/orders?x=1"} {
		get(t, client, srv.URL+path)
	}
	assert.Len(t, recorder.Ended(), 2, "only /orders is traced")

	cfg := httpConfig{}
	IgnoreEndpoint("*.example.com")(&cfg)
	IgnoreEndpoint("[", "/")(&cfg)
	for url, traced := range map[string]bool{
		"https://api.example.com/x":    false,
		"https://example.com/x":        true,
		"https://other.org/":           true,
		"https://api.example.com:8443": false,
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
		require.NoError(t, err)
		assert.Equal(t, traced, cfg.traced(req), url)
	}
}

func TestTransportOfADisabledTracer(t *testing.T) {
	t.Parallel()
	tracer := New(WithDisabled(true))
	assert.Same(t, http.DefaultTransport, tracer.Transport(nil))
	base := &http.Transport{}
	assert.Same(t, base, tracer.Transport(base))
	assert.Same(t, http.DefaultTransport, tracer.HTTPClient().Transport)
}
