package tracing

import (
	"net/http"
	"path"
	"slices"

	smithytracing "github.com/aws/smithy-go/tracing"
	"github.com/aws/smithy-go/tracing/smithyoteltracing"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
)

// Powertools Python patches the libraries the X-Ray SDK knows (botocore,
// requests, httplib, ...) so that their calls become subsegments. Go has no
// patching: a client is instrumented when it is built, with the
// instrumentation OpenTelemetry and the AWS SDK provide.

// AWSTracerProvider returns the tracer provider an AWS SDK for Go v2 client
// traces its calls with, so that every call is a span (an X-Ray subsegment)
// under the span of the context it is made with:
//
//	client := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
//		o.TracerProvider = tracer.AWSTracerProvider()
//	})
//
// It is the SDK's own OpenTelemetry adapter, smithyoteltracing, which
// replaces the deprecated otelaws instrumentation. A disabled tracer returns
// the SDK's no-op provider.
func (t *Tracer) AWSTracerProvider() smithytracing.TracerProvider {
	if t.disabled {
		return smithytracing.NopTracerProvider{}
	}
	return smithyoteltracing.Adapt(t.provider)
}

// HTTPOption configures the HTTP instrumentation of Transport and HTTPClient.
type HTTPOption func(*httpConfig)

type httpConfig struct {
	ignored []ignoredEndpoint
}

type ignoredEndpoint struct {
	hostname string
	urls     []string
}

// IgnoreEndpoint leaves untraced the requests to hostname whose path is one
// of urls, as Powertools Python's ignore_endpoint does. hostname is a glob
// ("*.example.com"); an empty hostname matches every host, and no urls match
// every path. The option may be repeated; a request matching any of them is
// not traced.
func IgnoreEndpoint(hostname string, urls ...string) HTTPOption {
	return func(c *httpConfig) {
		if hostname == "" && len(urls) == 0 {
			return
		}
		c.ignored = append(c.ignored, ignoredEndpoint{hostname: hostname, urls: urls})
	}
}

// traced is the otelhttp filter: it reports whether r is to be traced.
func (c *httpConfig) traced(r *http.Request) bool {
	for _, rule := range c.ignored {
		hostMatches := rule.hostname == ""
		if !hostMatches {
			matched, err := path.Match(rule.hostname, r.URL.Hostname())
			hostMatches = err == nil && matched
		}
		urlMatches := len(rule.urls) == 0 ||
			slices.Contains(rule.urls, r.URL.Path) || slices.Contains(rule.urls, r.URL.RequestURI())
		if hostMatches && urlMatches {
			return false
		}
	}
	return true
}

// Transport wraps base (http.DefaultTransport when nil) so that every request
// is a client span under the span of the request's context, and carries the
// X-Ray trace header to the server. A disabled tracer returns base as it is.
func (t *Tracer) Transport(base http.RoundTripper, opts ...HTTPOption) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if t.disabled {
		return base
	}
	cfg := httpConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	otelOpts := []otelhttp.Option{
		otelhttp.WithTracerProvider(t.provider),
		otelhttp.WithPropagators(xray.Propagator{}),
	}
	if len(cfg.ignored) > 0 {
		otelOpts = append(otelOpts, otelhttp.WithFilter(cfg.traced))
	}
	return otelhttp.NewTransport(base, otelOpts...)
}

// HTTPClient returns an http.Client whose requests are traced, as Transport
// describes. Requests must carry the invocation's context
// (http.NewRequestWithContext) to nest under its span.
func (t *Tracer) HTTPClient(opts ...HTTPOption) *http.Client {
	return &http.Client{Transport: t.Transport(nil, opts...)}
}
