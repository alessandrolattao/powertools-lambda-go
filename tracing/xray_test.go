package tracing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// The X-Ray UDP exporter writes one datagram per export: the X-Ray daemon
// protocol header, the OTLP traces prefix, and the base64 of an OTLP
// ExportTraceServiceRequest. This file plays the X-Ray endpoint Lambda runs
// next to a function, and checks what reaches it.
var (
	xrayProtocolHeader = []byte("{\"format\":\"json\",\"version\":1}\n")
	xrayTracesPrefix   = []byte("T1S")

	errNoProtocolHeader = errors.New("no X-Ray protocol header")
	errNotTraces        = errors.New("not an OTLP traces datagram")
)

// exportedSpan is a span as the X-Ray endpoint receives it, with the
// resource it came from.
type exportedSpan struct {
	*tracepb.Span

	resource map[string]*commonpb.AnyValue
}

func (s exportedSpan) attr(key string) *commonpb.AnyValue {
	for _, kv := range s.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

// xrayEndpoint keeps every span exported to it.
type xrayEndpoint struct {
	conn  *net.UDPConn
	mu    sync.Mutex
	spans []exportedSpan
	done  chan struct{}
}

func newXRayEndpoint(t *testing.T) *xrayEndpoint {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	x := &xrayEndpoint{conn: conn, done: make(chan struct{})}
	go x.receive()
	t.Cleanup(func() {
		assert.NoError(t, conn.Close())
		<-x.done
	})
	return x
}

func (x *xrayEndpoint) address() string {
	return x.conn.LocalAddr().String()
}

func (x *xrayEndpoint) receive() {
	defer close(x.done)
	buf := make([]byte, 64*1024)
	for {
		n, _, err := x.conn.ReadFromUDP(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue
		}
		spans, err := decodeXRayDatagram(buf[:n])
		if err != nil {
			continue
		}
		x.mu.Lock()
		x.spans = append(x.spans, spans...)
		x.mu.Unlock()
	}
}

func decodeXRayDatagram(datagram []byte) ([]exportedSpan, error) {
	payload, ok := bytes.CutPrefix(datagram, xrayProtocolHeader)
	if !ok {
		return nil, errNoProtocolHeader
	}
	payload, ok = bytes.CutPrefix(payload, xrayTracesPrefix)
	if !ok {
		return nil, errNotTraces
	}
	raw, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		return nil, err
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var spans []exportedSpan
	for _, rs := range req.GetResourceSpans() {
		res := map[string]*commonpb.AnyValue{}
		for _, kv := range rs.GetResource().GetAttributes() {
			res[kv.GetKey()] = kv.GetValue()
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				spans = append(spans, exportedSpan{Span: span, resource: res})
			}
		}
	}
	return spans, nil
}

// invocation waits for the spans of the invocation requestID: its server
// span, which otellambda tags with the request id, and the spans of its
// trace.
func (x *xrayEndpoint) invocation(t *testing.T, requestID string, spans int) (server exportedSpan, all []exportedSpan) {
	t.Helper()
	require.Eventually(t, func() bool {
		x.mu.Lock()
		defer x.mu.Unlock()
		i := slices.IndexFunc(x.spans, func(s exportedSpan) bool {
			return s.GetKind() == tracepb.Span_SPAN_KIND_SERVER &&
				s.attr("faas.invocation_id").GetStringValue() == requestID
		})
		if i < 0 {
			return false
		}
		server = x.spans[i]
		all = all[:0]
		for _, s := range x.spans {
			if bytes.Equal(s.GetTraceId(), server.GetTraceId()) {
				all = append(all, s)
			}
		}
		return len(all) == spans
	}, 5*time.Second, 10*time.Millisecond, "no spans for invocation %s", requestID)
	return server, all
}

// handle is the handler of the end-to-end test: it annotates the invocation,
// adds metadata, and charges the order in a captured function.
func handle(tracer *Tracer) func(context.Context, order) (receipt, error) {
	captured := tracer.CaptureMethod(charge)
	return func(ctx context.Context, o order) (receipt, error) {
		PutAnnotation(ctx, "order.id", o.ID)
		PutMetadata(ctx, "order", o, "")
		if _, err := captured(ctx, o); err != nil {
			return receipt{}, err
		}
		return receipt{OrderID: o.ID, Paid: true}, nil
	}
}

func TestXRayEndpointReceivesTheInvocation(t *testing.T) {
	endpoint := newXRayEndpoint(t)
	lambdaEnvironment(t)
	t.Setenv(DaemonAddressEnv, endpoint.address())
	t.Setenv(env.LambdaInitializationType, "on-demand")
	t.Setenv(env.XRayTraceID, "") // what aws-lambda-go leaves with concurrency above 1
	coldstart.Reset()
	t.Cleanup(coldstart.Reset)

	tracer := New(WithService("payment"), WithGlobals(false))
	require.NotNil(t, tracer.owned, "the tracer exports to the X-Ray endpoint")
	t.Cleanup(func() { assert.NoError(t, tracer.Shutdown(context.Background())) })
	handler := tracer.CaptureLambdaHandler(handle(tracer), WithName("handle"))

	got, err := handler(invocationContext(t, "req-ok", testHeader), order{ID: "o-1", Amount: 2})
	require.NoError(t, err)
	assert.True(t, got.Paid)

	server, spans := endpoint.invocation(t, "req-ok", 2)
	assert.Equal(t, functionName, server.GetName())
	assert.Equal(t, testTrace, hex.EncodeToString(server.GetTraceId()), "parented on the Lambda trace from the context")
	assert.Equal(t, testParent, hex.EncodeToString(server.GetParentSpanId()))
	assert.Equal(t, "payment", server.resource["service.name"].GetStringValue())
	assert.Equal(t, "aws", server.resource["cloud.provider"].GetStringValue())
	assert.Equal(t, "go", server.resource["telemetry.sdk.language"].GetStringValue())

	assert.True(t, server.attr(ColdStartAnnotation).GetBoolValue())
	assert.Equal(t, "payment", server.attr(ServiceAnnotation).GetStringValue())
	assert.Equal(t, "o-1", server.attr("order_id").GetStringValue())
	values := server.attr(AnnotationsAttribute).GetArrayValue().GetValues()
	listed := make([]string, 0, len(values))
	for _, v := range values {
		listed = append(listed, v.GetStringValue())
	}
	assert.ElementsMatch(t, []string{ColdStartAnnotation, ServiceAnnotation, "order_id"}, listed)

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(server.attr(MetadataAttributePrefix+"payment").GetStringValue()), &meta))
	assert.Equal(t, map[string]any{
		"order":           map[string]any{"id": "o-1", "amount": float64(2)},
		"handle response": map[string]any{"orderId": "o-1", "paid": true},
	}, meta)

	i := slices.IndexFunc(spans, func(s exportedSpan) bool { return s.GetName() == "## tracing.charge" })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, server.GetSpanId(), spans[i].GetParentSpanId(), "the captured function nests in the invocation")
	assert.Equal(t, tracepb.Span_SPAN_KIND_INTERNAL, spans[i].GetKind())

	// A second invocation, in another trace, fails.
	const failingHeader = "Root=1-5759e988-00000000000000000000000f;Parent=53995c3f42cd8ad8;Sampled=1"
	_, err = handler(invocationContext(t, "req-fail", failingHeader), order{ID: "o-2"})
	require.ErrorIs(t, err, errDeclined)

	server, spans = endpoint.invocation(t, "req-fail", 2)
	assert.False(t, server.attr(ColdStartAnnotation).GetBoolValue())
	assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, server.GetStatus().GetCode())
	assert.Equal(t, errDeclined.Error(), server.GetStatus().GetMessage())
	require.Len(t, server.GetEvents(), 1)
	assert.Equal(t, "exception", server.GetEvents()[0].GetName())
	for _, s := range spans {
		assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, s.GetStatus().GetCode(), "%s failed", s.GetName())
	}
}

func TestXRayEndpointUnsampledTrace(t *testing.T) {
	endpoint := newXRayEndpoint(t)
	lambdaEnvironment(t)
	t.Setenv(DaemonAddressEnv, endpoint.address())

	tracer := New(WithGlobals(false))
	t.Cleanup(func() { assert.NoError(t, tracer.Shutdown(context.Background())) })
	handler := tracer.CaptureLambdaHandler(pay)

	const unsampled = "Root=1-5759e988-bd862e3fe1be46a994272795;Parent=53995c3f42cd8ad8;Sampled=0"
	_, err := handler(invocationContext(t, "req-unsampled", unsampled), order{Amount: 1})
	require.NoError(t, err)
	_, err = handler(invocationContext(t, "req-sampled", testHeader), order{Amount: 1})
	require.NoError(t, err)

	endpoint.invocation(t, "req-sampled", 1)
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	for _, s := range endpoint.spans {
		assert.NotEqual(t, "req-unsampled", s.attr("faas.invocation_id").GetStringValue(),
			"Lambda's sampling decision is kept")
	}
	assert.Len(t, endpoint.spans, 1, strconv.Itoa(len(endpoint.spans))+" spans")
}
