package tracing

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/aws-observability/aws-otel-go/exporters/xrayudp"
	lambdadetector "go.opentelemetry.io/contrib/detectors/aws/lambda"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-lambda-go/otellambda"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-lambda-go/otellambda/xrayconfig"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// Environment variables the tracer reads besides the POWERTOOLS_* ones.
const (
	// DaemonAddressEnv is where Lambda says the X-Ray endpoint listens, when
	// the function has active tracing on.
	DaemonAddressEnv = "AWS_XRAY_DAEMON_ADDRESS"
)

// instrumentation is the instrumentation scope of the spans the tracer opens.
const instrumentation = "github.com/alessandrolattao/powertools-lambda-go/tracing"

// Tracer records what a Lambda function does as OpenTelemetry spans, exported
// to AWS X-Ray. Build one per process with New, usually in main, and share
// it: it is safe for concurrent invocations.
type Tracer struct {
	provider trace.TracerProvider
	tracer   trace.Tracer
	// owned is the provider New built, which Shutdown releases. A provider
	// passed with WithTracerProvider belongs to the caller.
	owned    *sdktrace.TracerProvider
	service  string
	disabled bool
	lambda   []otellambda.Option
}

// Option configures a Tracer.
type Option func(*config)

type config struct {
	service  *string
	disabled *bool
	provider trace.TracerProvider
	globals  bool
}

// WithService names the service the traces belong to: the Service
// annotation of every invocation, the namespace of the captured metadata and
// the service.name of the spans. It defaults to POWERTOOLS_SERVICE_NAME; with
// neither, invocations carry no Service annotation, as in Powertools Python.
func WithService(name string) Option {
	return func(c *config) { c.service = &name }
}

// WithDisabled turns tracing off, or on, regardless of the environment. Left
// unset, tracing is off when POWERTOOLS_TRACE_DISABLED is true, outside
// Lambda (LAMBDA_TASK_ROOT unset), under the AWS SAM CLI (AWS_SAM_LOCAL set)
// and when Lambda gives the function no X-Ray endpoint (AWS_XRAY_DAEMON_ADDRESS
// unset, active tracing off). Forced on without an endpoint, spans go to
// 127.0.0.1:2000, where a local X-Ray daemon or collector listens.
func WithDisabled(disabled bool) Option {
	return func(c *config) { c.disabled = &disabled }
}

// WithTracerProvider makes the tracer record its spans with tp instead of
// building its own X-Ray provider. Tests pass one that records spans in
// memory. The environment detection of WithDisabled does not apply to it:
// only WithDisabled and POWERTOOLS_TRACE_DISABLED turn it off. Shutdown leaves
// tp alone; it belongs to the caller.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.provider = tp }
}

// WithGlobals decides whether New registers its tracer provider and the X-Ray
// propagator as the OpenTelemetry global ones, so that otelhttp, the AWS SDK
// and every otel.Tracer in the process join the same traces. It defaults to
// true; a disabled tracer never touches the globals.
func WithGlobals(enabled bool) Option {
	return func(c *config) { c.globals = enabled }
}

// New returns a Tracer. Unless tracing is disabled (see WithDisabled) or a
// provider is passed (WithTracerProvider), it builds the tracer provider the
// AWS X-Ray migration guide prescribes for Lambda: the X-Ray UDP exporter
// writing to AWS_XRAY_DAEMON_ADDRESS through a simple span processor (Lambda
// freezes the environment between invocations, so nothing waits in a batch),
// X-Ray trace ids, and the Lambda resource with service.name set to the
// service.
//
// New never fails: tracing that cannot be set up is reported on standard
// error and left off, so that it never stops a function from starting.
func New(opts ...Option) *Tracer {
	cfg := config{globals: true}
	for _, opt := range opts {
		opt(&cfg)
	}

	t := &Tracer{service: env.Get(env.ServiceName)}
	if cfg.service != nil {
		t.service = *cfg.service
	}

	disabled, reason := cfg.isDisabled()
	switch {
	case disabled:
		debug.Logger().Debug("tracing disabled", "reason", reason)
	case cfg.provider != nil:
		t.provider = cfg.provider
	default:
		tp, err := newProvider(context.Background(), t.service, daemonAddress())
		if err != nil {
			// Reported on standard error, where Lambda collects it.
			slog.New(slog.NewJSONHandler(os.Stderr, nil)).Warn(
				"powertools: tracing is off, the X-Ray exporter could not start", "err", err)
			disabled = true
			break
		}
		t.provider, t.owned = tp, tp
	}

	if disabled {
		t.disabled = true
		t.provider = noop.NewTracerProvider()
	}
	t.tracer = t.provider.Tracer(instrumentation)
	t.lambda = t.lambdaOptions()

	if !t.disabled && cfg.globals {
		otel.SetTracerProvider(t.provider)
		otel.SetTextMapPropagator(xray.Propagator{})
	}
	return t
}

// isDisabled applies Powertools Python's rules: an explicit choice wins,
// then POWERTOOLS_TRACE_DISABLED, then the environment. A provider passed in
// is the caller's decision to trace, so the environment does not veto it.
func (c *config) isDisabled() (disabled bool, reason string) {
	switch {
	case c.disabled != nil:
		return *c.disabled, "WithDisabled"
	case env.Bool(env.TraceDisabled, false):
		return true, env.TraceDisabled
	case c.provider != nil:
		return false, ""
	case env.Get(env.LambdaTaskRoot) == "":
		return true, "not running in AWS Lambda"
	case env.Get(env.SAMLocal) != "":
		return true, "running in the AWS SAM CLI"
	case env.Get(DaemonAddressEnv) == "":
		return true, "no X-Ray endpoint: active tracing is off"
	default:
		return false, ""
	}
}

// daemonAddress returns the UDP address of the X-Ray endpoint. The variable
// may also hold the X-Ray SDKs' two-address form, "tcp:host:port
// udp:host:port", of which the exporter needs the UDP one.
func daemonAddress() string {
	return parseDaemonAddress(env.Get(DaemonAddressEnv))
}

func parseDaemonAddress(raw string) string {
	fields := strings.Fields(raw)
	for _, field := range fields {
		if address, ok := strings.CutPrefix(field, "udp:"); ok && address != "" {
			return address
		}
	}
	if len(fields) == 1 && !strings.HasPrefix(fields[0], "tcp:") {
		return fields[0]
	}
	return xrayudp.DefaultEndpoint
}

// newProvider builds the tracer provider of the AWS guide "Migrate to
// OpenTelemetry Go", section "Lambda manual instrumentation".
func newProvider(ctx context.Context, service, address string) (*sdktrace.TracerProvider, error) {
	res, err := newResource(ctx, service)
	if err != nil {
		return nil, err
	}
	exporter, err := xrayudp.NewSpanExporter(ctx, xrayudp.WithEndpoint(address))
	if err != nil {
		return nil, fmt.Errorf("tracing: X-Ray UDP exporter to %q: %w", address, err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(xray.NewIDGenerator()),
	), nil
}

// newResource describes the function: the Lambda attributes, the SDK's
// (telemetry.sdk.language, which X-Ray needs to show stack traces), those of
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME, and service.name. The
// service name is the Powertools service; without one, a name set through
// the OpenTelemetry variables is kept, else the function's name is used.
func newResource(ctx context.Context, service string) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithDetectors(lambdadetector.NewResourceDetector()),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	// A malformed OTEL_RESOURCE_ATTRIBUTES loses its attributes, not the
	// rest of the resource.
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("tracing: describe the resource: %w", err)
	}
	name := service
	if _, named := res.Set().Value(semconv.ServiceNameKey); name == "" && !named {
		name = env.Get(env.LambdaFunctionName)
	}
	if name == "" {
		return res, nil
	}
	res, err = resource.Merge(res, resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(name)))
	if err != nil {
		return nil, fmt.Errorf("tracing: name the service: %w", err)
	}
	return res, nil
}

// lambdaOptions configures otellambda the way xrayconfig recommends, except
// for where the parent of the invocation span comes from: xrayconfig reads
// _X_AMZN_TRACE_ID, which aws-lambda-go sets only while the environment runs
// one invocation at a time, so the tracer puts the parent in the context
// itself (withLambdaParent) and otellambda reads no carrier.
func (t *Tracer) lambdaOptions() []otellambda.Option {
	var opts []otellambda.Option
	if t.owned != nil {
		opts = xrayconfig.WithRecommendedOptions(t.owned)
	} else {
		opts = []otellambda.Option{otellambda.WithTracerProvider(t.provider), xrayconfig.WithPropagator()}
		if flusher, ok := t.provider.(otellambda.Flusher); ok {
			opts = append(opts, otellambda.WithFlusher(flusher))
		}
	}
	return append(opts, otellambda.WithEventToCarrier(noCarrier))
}

func noCarrier([]byte) propagation.TextMapCarrier {
	return propagation.HeaderCarrier{}
}

// Provider returns the OpenTelemetry tracer provider the tracer records with:
// the escape hatch to everything OpenTelemetry offers that the tracer does
// not wrap. A disabled tracer returns a no-op provider.
func (t *Tracer) Provider() trace.TracerProvider {
	return t.provider
}

// ForceFlush exports the spans the tracer provider still holds. The handler
// decorator does it at the end of every invocation already.
func (t *Tracer) ForceFlush(ctx context.Context) error {
	flusher, ok := t.provider.(otellambda.Flusher)
	if !ok {
		return nil
	}
	if err := flusher.ForceFlush(ctx); err != nil {
		return fmt.Errorf("tracing: flush the spans: %w", err)
	}
	return nil
}

// Shutdown flushes and releases the tracer provider New built. A Lambda
// function never needs it, since every invocation flushes its spans; a
// program that outlives its handler (a task, a CLI) calls it before exiting.
// A provider passed with WithTracerProvider is left alone.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t.owned == nil {
		return nil
	}
	if err := t.owned.Shutdown(ctx); err != nil {
		return fmt.Errorf("tracing: shut down the tracer provider: %w", err)
	}
	return nil
}

// serviceKey carries the service of the tracer that captured the current
// function, the default metadata namespace of PutMetadata.
type serviceKey struct{}

func withService(ctx context.Context, service string) context.Context {
	if service == "" {
		return ctx
	}
	return context.WithValue(ctx, serviceKey{}, service)
}

// serviceOf returns the service the metadata of ctx belongs to by default:
// the one of the tracer that captured the current function, else
// POWERTOOLS_SERVICE_NAME, else X-Ray's default namespace.
func serviceOf(ctx context.Context) string {
	if service, ok := ctx.Value(serviceKey{}).(string); ok && service != "" {
		return service
	}
	return cmp.Or(env.Get(env.ServiceName), DefaultNamespace)
}
