package tracing_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	"github.com/aws/aws-lambda-go/lambdacontext"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/alessandrolattao/powertools-lambda-go/tracing"
)

type Order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

type Receipt struct {
	OrderID string `json:"orderId"`
	Paid    bool   `json:"paid"`
}

// recording returns a tracer whose spans stay in memory, as a test of your
// own would build it.
func recording() (tracer *tracing.Tracer, recorder *tracetest.SpanRecorder) {
	recorder = tracetest.NewSpanRecorder()
	tracer = tracing.New(
		tracing.WithService("payment"),
		tracing.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))),
		tracing.WithGlobals(false),
	)
	return tracer, recorder
}

// xray lists the X-Ray annotations and metadata of span, sorted.
func xray(span sdktrace.ReadOnlySpan) string {
	var lines []string
	for _, kv := range span.Attributes() {
		key := string(kv.Key)
		switch {
		case key == tracing.ColdStartAnnotation: // true or false depending on the run
		case key == tracing.AnnotationsAttribute:
			listed := slices.DeleteFunc(kv.Value.AsStringSlice(), func(k string) bool { return k == tracing.ColdStartAnnotation })
			slices.Sort(listed)
			lines = append(lines, fmt.Sprintf("%s: %v", key, listed))
		case strings.HasPrefix(key, tracing.MetadataAttributePrefix), key == tracing.ServiceAnnotation, key == "order_id":
			lines = append(lines, fmt.Sprintf("%s: %s", key, kv.Value.String()))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func ExampleTracer_CaptureLambdaHandler() {
	tracer, recorder := recording()

	handle := func(ctx context.Context, o Order) (Receipt, error) {
		tracing.PutAnnotation(ctx, "order_id", o.ID)
		return Receipt{OrderID: o.ID, Paid: true}, nil
	}
	// In main: lambda.Start(tracer.CaptureLambdaHandler(handle)).
	handler := tracer.CaptureLambdaHandler(handle, tracing.WithName("handle"))

	ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{AwsRequestID: "req-1"})
	if _, err := handler(ctx, Order{ID: "o-1", Amount: 5}); err != nil {
		panic(err)
	}
	fmt.Println(xray(recorder.Ended()[0]))
	// Output:
	// Service: payment
	// aws.xray.annotations: [Service order_id]
	// aws.xray.metadata.payment: {"handle response":{"orderId":"o-1","paid":true}}
	// order_id: o-1
}

func ExampleTracer_CaptureMethod() {
	tracer, recorder := recording()

	charge := tracer.CaptureMethod(func(_ context.Context, o Order) (int, error) {
		return o.Amount * 100, nil
	}, tracing.WithName("charge"))

	cents, err := charge(context.Background(), Order{ID: "o-1", Amount: 5})
	if err != nil {
		panic(err)
	}
	span := recorder.Ended()[0]
	fmt.Println(cents, span.Name())
	fmt.Println(xray(span))
	// Output:
	// 500 ## charge
	// aws.xray.metadata.payment: {"charge response":500}
}

func ExampleTracer_Capture() {
	tracer, recorder := recording()

	total, err := tracer.Capture(context.Background(), "sum", func(ctx context.Context) (int, error) {
		tracing.PutMetadata(ctx, "lines", []int{1, 2, 3}, "cart")
		return 6, nil
	}, tracing.WithCaptureResponse(false))
	if err != nil {
		panic(err)
	}
	span := recorder.Ended()[0]
	fmt.Println(total, span.Name())
	fmt.Println(xray(span))
	// Output:
	// 6 ## sum
	// aws.xray.metadata.cart: {"lines":[1,2,3]}
}

func ExamplePutAnnotation() {
	tracer, recorder := recording()

	_, err := tracer.Capture(context.Background(), "confirm", func(ctx context.Context) (struct{}, error) {
		tracing.PutAnnotation(ctx, "order.id", "o-1") // stored as order_id
		tracing.PutAnnotation(ctx, "amount", 5)
		tracing.PutAnnotation(ctx, "express", true)
		return struct{}{}, nil
	})
	if err != nil {
		panic(err)
	}
	span := recorder.Ended()[0]
	for _, kv := range span.Attributes() {
		fmt.Println(kv.Key, kv.Value.String())
	}
	// Output:
	// order_id o-1
	// aws.xray.annotations ["order_id","amount","express"]
	// amount 5
	// express true
}

func ExampleNew() {
	// Outside Lambda, without active tracing, the tracer is off: the
	// decorators return the functions they get and nothing is exported.
	tracer := tracing.New()

	_, span := tracer.Provider().Tracer("example").Start(context.Background(), "probe")
	defer span.End()
	fmt.Println("recording:", span.IsRecording())
	// Output: recording: false
}

func ExampleTracer_HTTPClient() {
	tracer, recorder := recording()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("trace header sent:", r.Header.Get("X-Amzn-Trace-Id") != "")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()

	client := tracer.HTTPClient(tracing.IgnoreEndpoint("", "/health"))
	for _, path := range []string{"/orders", "/health"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL+path, http.NoBody)
		if err != nil {
			panic(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}
		_ = resp.Body.Close() //nolint:errcheck // an empty body
	}
	fmt.Println("spans:", len(recorder.Ended()))
	// Output:
	// trace header sent: true
	// trace header sent: false
	// spans: 1
}
