package logging_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
)

// clock makes the examples' timestamps stable.
func clock() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 123_000_000, time.UTC) }

func ExampleNew() {
	var out strings.Builder
	logger := logging.New(logging.WithService("payment"), logging.WithWriter(&out), logging.WithClock(clock))

	logger.InfoContext(context.Background(), "collecting payment", "charge_id", "ch_42")
	logger.ErrorContext(context.Background(), "charge failed", "err", errors.New("card declined"))

	fmt.Print(out.String())
	// Output:
	// {"level":"INFO","location":"logging_test.ExampleNew:23","message":"collecting payment","timestamp":"2026-10-10T12:00:00.123Z","service":"payment","charge_id":"ch_42"}
	// {"level":"ERROR","location":"logging_test.ExampleNew:24","message":"charge failed","timestamp":"2026-10-10T12:00:00.123Z","service":"payment","exception":"card declined","exception_name":"error"}
}

func ExampleLogger_InjectContext() {
	var out strings.Builder
	logger := logging.New(logging.WithService("orders"), logging.WithWriter(&out), logging.WithClock(clock),
		logging.WithoutKeys(logging.KeyLocation))

	handle := func(ctx context.Context, request events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		logging.AppendKeys(ctx, "route", request.RouteKey)
		logger.InfoContext(ctx, "listing orders")
		return events.APIGatewayV2HTTPResponse{StatusCode: 200}, nil
	}
	// In main: lambda.Start(logger.InjectContext(handle, ...)).
	wrapped := logger.InjectContext(handle, logging.WithCorrelationIDPath(logging.CorrelationAPIGatewayHTTP))

	_, err := wrapped(context.Background(), events.APIGatewayV2HTTPRequest{
		RouteKey:       "GET /orders",
		RequestContext: events.APIGatewayV2HTTPRequestContext{RequestID: "r-1"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"level":"INFO","message":"listing orders","timestamp":"2026-10-10T12:00:00.123Z","service":"orders","correlation_id":"r-1","route":"GET /orders"}
}

func ExampleWithLogEvent() {
	var out strings.Builder
	logger := logging.New(logging.WithService("orders"), logging.WithWriter(&out), logging.WithClock(clock),
		logging.WithoutKeys(logging.KeyLocation))

	type order struct {
		ID string `json:"id"`
	}
	handle := logger.InjectContext(func(context.Context, order) (string, error) {
		return "accepted", nil
	}, logging.WithLogEvent(true))

	if _, err := handle(context.Background(), order{ID: "o-1"}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"level":"INFO","message":{"id":"o-1"},"timestamp":"2026-10-10T12:00:00.123Z","service":"orders"}
}

func ExampleWithKeys() {
	var out strings.Builder
	logger := logging.New(logging.WithService("orders"), logging.WithWriter(&out), logging.WithClock(clock),
		logging.WithoutKeys(logging.KeyLocation))

	ctx := logging.WithKeys(context.Background(), "step", "charge")
	logger.InfoContext(ctx, "charging")
	logger.InfoContext(context.Background(), "done")

	fmt.Print(out.String())
	// Output:
	// {"level":"INFO","message":"charging","timestamp":"2026-10-10T12:00:00.123Z","service":"orders","step":"charge"}
	// {"level":"INFO","message":"done","timestamp":"2026-10-10T12:00:00.123Z","service":"orders"}
}

func ExampleWithBuffer() {
	var out strings.Builder
	logger := logging.New(logging.WithService("orders"), logging.WithWriter(&out), logging.WithClock(clock),
		logging.WithoutKeys(logging.KeyLocation), logging.WithBuffer())

	handle := logger.InjectContext(func(ctx context.Context, id string) (string, error) {
		logger.DebugContext(ctx, "loading order", "id", id) // buffered
		if id == "" {
			logger.ErrorContext(ctx, "no order id") // flushes the buffer first
		}
		return id, nil
	})
	for _, id := range []string{"o-1", ""} { // the first invocation's debug record is dropped with it
		if _, err := handle(context.Background(), id); err != nil {
			panic(err)
		}
	}

	fmt.Print(out.String())
	// Output:
	// {"level":"DEBUG","message":"loading order","timestamp":"2026-10-10T12:00:00.123Z","service":"orders","id":""}
	// {"level":"ERROR","message":"no order id","timestamp":"2026-10-10T12:00:00.123Z","service":"orders"}
}

func ExampleNewHandler() {
	var out strings.Builder
	handler := logging.NewHandler(logging.WithService("orders"), logging.WithWriter(&out), logging.WithClock(clock),
		logging.WithoutKeys(logging.KeyLocation))

	// slog.SetDefault(slog.New(handler)) sends slog's default logger, and the
	// standard log package, through Powertools.
	logger := slog.New(handler)
	logger.Info("from slog", "library", "any")

	fmt.Print(out.String())
	// Output:
	// {"level":"INFO","message":"from slog","timestamp":"2026-10-10T12:00:00.123Z","service":"orders","library":"any"}
}

func ExampleWithLevelNames() {
	var out strings.Builder
	logger := logging.New(logging.WithService("orders"), logging.WithWriter(&out), logging.WithClock(clock),
		logging.WithoutKeys(logging.KeyLocation),
		logging.WithLevelNames(logging.PythonLevelName), logging.WithTimestampFormat(logging.TimestampPython))

	logger.WarnContext(context.Background(), "as Powertools Python writes it")

	fmt.Print(out.String())
	// Output:
	// {"level":"WARNING","message":"as Powertools Python writes it","timestamp":"2026-10-10 12:00:00,123+0000","service":"orders"}
}
