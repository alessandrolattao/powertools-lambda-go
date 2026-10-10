package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// otelServiceNameEnv names the service the OpenTelemetry way.
const otelServiceNameEnv = "OTEL_SERVICE_NAME"

// lambdaEnvironment sets what Lambda sets for a function with active tracing.
func lambdaEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv(env.LambdaTaskRoot, "/var/task")
	t.Setenv(env.LambdaFunctionName, "payment-fn")
	t.Setenv(DaemonAddressEnv, "127.0.0.1:2000")
	t.Setenv(env.SAMLocal, "")
	t.Setenv(env.TraceDisabled, "")
}

// keepGlobals resets the OpenTelemetry globals when the test ends.
func keepGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
}

func TestNewDecidesWhetherToTrace(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		opts     []Option
		disabled bool
	}{
		{name: "in Lambda with active tracing", disabled: false},
		{name: "POWERTOOLS_TRACE_DISABLED", env: map[string]string{env.TraceDisabled: "true"}, disabled: true},
		{name: "POWERTOOLS_TRACE_DISABLED off", env: map[string]string{env.TraceDisabled: "0"}, disabled: false},
		{name: "outside Lambda", env: map[string]string{env.LambdaTaskRoot: ""}, disabled: true},
		{name: "SAM CLI", env: map[string]string{env.SAMLocal: "true"}, disabled: true},
		{name: "no X-Ray endpoint", env: map[string]string{DaemonAddressEnv: ""}, disabled: true},
		{name: "explicitly off", opts: []Option{WithDisabled(true)}, disabled: true},
		{
			name:     "explicitly on outside Lambda",
			env:      map[string]string{env.LambdaTaskRoot: "", DaemonAddressEnv: ""},
			opts:     []Option{WithDisabled(false)},
			disabled: false,
		},
		{
			name:     "explicitly on beats POWERTOOLS_TRACE_DISABLED",
			env:      map[string]string{env.TraceDisabled: "true"},
			opts:     []Option{WithDisabled(false)},
			disabled: false,
		},
		{
			name:     "a provider passed in is used outside Lambda",
			env:      map[string]string{env.LambdaTaskRoot: ""},
			opts:     []Option{WithTracerProvider(sdktrace.NewTracerProvider())},
			disabled: false,
		},
		{
			name:     "POWERTOOLS_TRACE_DISABLED beats a provider passed in",
			env:      map[string]string{env.TraceDisabled: "true"},
			opts:     []Option{WithTracerProvider(sdktrace.NewTracerProvider())},
			disabled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lambdaEnvironment(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			tracer := New(append(tt.opts, WithGlobals(false))...)
			t.Cleanup(func() { assert.NoError(t, tracer.Shutdown(context.Background())) })

			assert.Equal(t, tt.disabled, tracer.disabled)
			_, span := tracer.Provider().Tracer("test").Start(t.Context(), "probe")
			defer span.End()
			assert.Equal(t, !tt.disabled, span.IsRecording())
		})
	}
}

func TestNewResolvesTheService(t *testing.T) {
	t.Setenv(env.ServiceName, "from-env")
	assert.Equal(t, "from-env", New(WithDisabled(true)).service)
	assert.Equal(t, "explicit", New(WithDisabled(true), WithService("explicit")).service)
	assert.Empty(t, New(WithDisabled(true), WithService("")).service, "an explicit empty service is a choice")
}

func TestNewRegistersTheGlobals(t *testing.T) { //nolint:paralleltest // it changes the environment and the OpenTelemetry globals
	lambdaEnvironment(t)
	keepGlobals(t)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	tracer := New()
	t.Cleanup(func() { assert.NoError(t, tracer.Shutdown(context.Background())) })

	assert.Same(t, tracer.Provider(), otel.GetTracerProvider())
	assert.IsType(t, xray.Propagator{}, otel.GetTextMapPropagator())
}

func TestNewLeavesTheGlobalsAlone(t *testing.T) { //nolint:paralleltest // it changes the environment and the OpenTelemetry globals
	keepGlobals(t)
	before := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(before)

	New(WithTracerProvider(sdktrace.NewTracerProvider()), WithGlobals(false))
	assert.Same(t, before, otel.GetTracerProvider())

	lambdaEnvironment(t)
	New(WithDisabled(true))
	assert.Same(t, before, otel.GetTracerProvider(), "a disabled tracer registers nothing")
}

func TestNewNamesTheService(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		opts    []Option
		service string
	}{
		{name: "the Powertools service", opts: []Option{WithService("payment")}, service: "payment"},
		{name: "POWERTOOLS_SERVICE_NAME", env: map[string]string{env.ServiceName: "billing"}, service: "billing"},
		{name: "the function without a service", service: "payment-fn"},
		{name: "OTEL_SERVICE_NAME without a service", env: map[string]string{otelServiceNameEnv: "otel-name"}, service: "otel-name"},
		{
			name:    "the Powertools service beats OTEL_SERVICE_NAME",
			env:     map[string]string{otelServiceNameEnv: "otel-name"},
			opts:    []Option{WithService("payment")},
			service: "payment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lambdaEnvironment(t)
			t.Setenv(env.ServiceName, "")
			t.Setenv(otelServiceNameEnv, "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			tracer := New(append(tt.opts, WithGlobals(false))...)
			t.Cleanup(func() { assert.NoError(t, tracer.Shutdown(context.Background())) })
			require.NotNil(t, tracer.owned, "New builds its own provider in Lambda")

			_, span := tracer.Provider().Tracer("test").Start(t.Context(), "probe")
			span.End()
			ro, ok := span.(sdktrace.ReadOnlySpan)
			require.True(t, ok)
			name, found := ro.Resource().Set().Value(semconv.ServiceNameKey)
			require.True(t, found)
			assert.Equal(t, tt.service, name.AsString())

			faas, found := ro.Resource().Set().Value(semconv.FaaSNameKey)
			require.True(t, found, "the Lambda resource is detected")
			assert.Equal(t, "payment-fn", faas.AsString())
			lang, found := ro.Resource().Set().Value(semconv.TelemetrySDKLanguageKey)
			require.True(t, found, "X-Ray needs the SDK language to show stack traces")
			assert.Equal(t, "go", lang.AsString())
		})
	}
}

func TestNewUsesXRayTraceIDs(t *testing.T) { //nolint:paralleltest // lambdaEnvironment changes the environment
	lambdaEnvironment(t)
	tracer := New(WithGlobals(false))
	t.Cleanup(func() { assert.NoError(t, tracer.Shutdown(context.Background())) })

	_, span := tracer.Provider().Tracer("test").Start(t.Context(), "probe")
	span.End()
	id := span.SpanContext().TraceID()
	assert.NotEqual(t, trace.TraceID{}, id)
	// An X-Ray trace id starts with the epoch seconds of its creation.
	assert.NotZero(t, uint32(id[0])<<24|uint32(id[1])<<16|uint32(id[2])<<8|uint32(id[3]))
}

func TestNewWithAnUnusableEndpointTurnsTracingOff(t *testing.T) {
	lambdaEnvironment(t)
	t.Setenv(DaemonAddressEnv, "not-an-address")
	tracer := New(WithGlobals(false))
	assert.True(t, tracer.disabled, "a broken endpoint never stops the function")
}

func TestParseDaemonAddress(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"169.254.79.129:2000":                     "169.254.79.129:2000",
		" 169.254.79.129:2000 ":                   "169.254.79.129:2000",
		"tcp:127.0.0.1:2000 udp:127.0.0.2:2001":   "127.0.0.2:2001",
		"udp:127.0.0.2:2001 tcp:127.0.0.1:2000":   "127.0.0.2:2001",
		"":                                        "127.0.0.1:2000",
		"tcp:127.0.0.1:2000":                      "127.0.0.1:2000",
		"tcp:127.0.0.1:2000 tcp:127.0.0.1:2001 x": "127.0.0.1:2000",
	}
	for raw, want := range tests {
		assert.Equal(t, want, parseDaemonAddress(raw), "%q", raw)
	}
}

func TestShutdownLeavesAProvidedProviderAlone(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	require.NoError(t, tracer.Shutdown(t.Context()))
	require.NoError(t, tracer.ForceFlush(t.Context()))

	_, span := tracer.Provider().Tracer("test").Start(t.Context(), "after shutdown")
	span.End()
	assert.Len(t, recorder.Ended(), 1, "the caller's provider still works")
}

func TestADisabledTracerHasNothingToFlush(t *testing.T) {
	t.Parallel()
	tracer := New(WithDisabled(true))
	require.NoError(t, tracer.ForceFlush(t.Context()))
	require.NoError(t, tracer.Shutdown(t.Context()))
}
