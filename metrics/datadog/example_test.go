package datadog_test

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/metrics/datadog"
)

type order struct {
	Product string `json:"product"`
}

func ExampleNew() {
	// The example writes Datadog Forwarder lines to a buffer on a fixed
	// clock; a function leaves WithWriter and WithClock out.
	var out bytes.Buffer
	m := datadog.New(
		datadog.WithNamespace("ServerlessAirline"),
		datadog.WithFlushToLog(true),
		datadog.WithDefaultTag("environment", "prod"),
		datadog.WithWriter(&out),
		datadog.WithClock(func() time.Time { return time.Unix(1700000000, 0) }),
	)

	handle := func(ctx context.Context, o order) (string, error) {
		return o.Product, datadog.AddMetric(ctx, "item_sold", 1, datadog.WithTag("product", o.Product))
	}
	// A function hands the decorated handler to lambda.Start.
	h := m.LogMetrics(handle)

	if _, err := h(context.Background(), order{Product: "latte"}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"m":"ServerlessAirline.item_sold","v":1,"e":1700000000,"t":["environment:prod","product:latte"]}
}

func ExampleAddMetric() {
	var out bytes.Buffer
	m := datadog.New(datadog.WithFlushToLog(true), datadog.WithWriter(&out))
	h := m.LogMetrics(func(ctx context.Context, _ order) (string, error) {
		// A metric that happened earlier, timestamped to the second.
		return "", datadog.AddMetric(ctx, "order_value", 12.45,
			datadog.WithTag("order", "online"),
			datadog.WithTimestamp(time.Unix(1691768022, 0)))
	}, metrics.WithDefaultDimensions(metrics.Dimension{Name: "team", Value: "coffee"}))

	if _, err := h(context.Background(), order{}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"m":"order_value","v":12.45,"e":1691768022,"t":["team:coffee","order:online"]}
}
