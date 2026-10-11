package metrics_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/metrics"
)

type booking struct {
	ID string `json:"id"`
}

// The examples write to a buffer on a fixed clock, so that their documents
// are reproducible. A function leaves both options out: documents go to
// standard output, timestamped now.
func exampleMetrics(opts ...metrics.Option) (*metrics.Metrics, *bytes.Buffer) {
	var out bytes.Buffer
	clock := func() time.Time { return time.UnixMilli(1700000000000) }
	opts = append([]metrics.Option{metrics.WithWriter(&out), metrics.WithClock(clock)}, opts...)
	return metrics.New(opts...), &out
}

func ExampleMetrics_LogMetrics() {
	m, out := exampleMetrics(metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking"))

	handle := func(ctx context.Context, b booking) (string, error) {
		if err := metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1); err != nil {
			return "", err
		}
		return b.ID, nil
	}
	// A function hands the decorated handler to lambda.Start.
	h := m.LogMetrics(handle)

	if _, err := h(context.Background(), booking{ID: "b-1"}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["service"]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"service":"booking","SuccessfulBooking":[1.0]}
}

func ExampleAddMetric() {
	m, out := exampleMetrics(metrics.WithNamespace("ServerlessAirline"))
	h := m.LogMetrics(func(ctx context.Context, _ booking) (string, error) {
		// The same name collects several values; high resolution metrics
		// keep one-second data points.
		for _, reads := range []float64{1, 8} {
			if err := metrics.AddMetric(ctx, "TurbineReads", metrics.Count, reads); err != nil {
				return "", err
			}
		}
		return "", metrics.AddMetric(ctx, "Latency", metrics.Milliseconds, 12.5, metrics.WithResolution(metrics.High))
	})

	if _, err := h(context.Background(), booking{}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[[]],"Metrics":[{"Name":"TurbineReads","Unit":"Count"},{"Name":"Latency","Unit":"Milliseconds","StorageResolution":1}]}]},"TurbineReads":[1.0,8.0],"Latency":[12.5]}
}

func ExampleAddDimensions() {
	m, out := exampleMetrics(metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking"))
	h := m.LogMetrics(func(ctx context.Context, _ booking) (string, error) {
		// Aggregate the metric by environment and region, and by
		// environment alone, in one document.
		err := metrics.AddDimensions(ctx,
			metrics.Dimension{Name: "environment", Value: "prod"},
			metrics.Dimension{Name: "region", Value: "us-east-1"})
		if err != nil {
			return "", err
		}
		if err := metrics.AddDimensions(ctx, metrics.Dimension{Name: "environment", Value: "prod"}); err != nil {
			return "", err
		}
		return "", metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
	})

	if _, err := h(context.Background(), booking{}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["service"],["environment","region"],["environment"]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"service":"booking","environment":"prod","region":"us-east-1","SuccessfulBooking":[1.0]}
}

func ExampleAddMetadata() {
	m, out := exampleMetrics(metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking"))
	h := m.LogMetrics(func(ctx context.Context, b booking) (string, error) {
		if err := metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1); err != nil {
			return "", err
		}
		// Searchable in CloudWatch Logs Insights, not a dimension.
		return "", metrics.AddMetadata(ctx, "booking_id", b.ID)
	})

	if _, err := h(context.Background(), booking{ID: "00347014-341d-4b8e-8421-a89d3d588ab3"}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["service"]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"service":"booking","booking_id":"00347014-341d-4b8e-8421-a89d3d588ab3","SuccessfulBooking":[1.0]}
}

func ExampleWithDefaultDimensions() {
	m, out := exampleMetrics(
		metrics.WithNamespace("ServerlessAirline"),
		metrics.WithService("booking"),
		metrics.WithDefaultDimensions(metrics.Dimension{Name: "environment", Value: "prod"}),
	)
	h := m.LogMetrics(func(ctx context.Context, _ booking) (string, error) {
		return "", metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
	})

	if _, err := h(context.Background(), booking{}); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["environment","service"]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"environment":"prod","service":"booking","SuccessfulBooking":[1.0]}
}

func ExampleMetrics_SingleMetric() {
	m, out := exampleMetrics(metrics.WithNamespace("ServerlessAirline"), metrics.WithService("booking"))

	// A metric with dimensions and a timestamp of its own, in a document of
	// its own.
	err := m.SingleMetric("RecordsCount", metrics.Count, 10).
		AddDimension("TableName", "Users").
		SetTimestamp(time.UnixMilli(1699999000000)).
		Flush()
	if err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1699999000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[["TableName","service"]],"Metrics":[{"Name":"RecordsCount","Unit":"Count"}]}]},"TableName":"Users","service":"booking","RecordsCount":[10.0]}
}

func ExampleMetrics_NewContext() {
	m, out := exampleMetrics(metrics.WithNamespace("ServerlessAirline"))

	// Without LogMetrics, a handler installs a metric set and flushes it,
	// also when the work fails.
	work := func(ctx context.Context) (err error) {
		ctx = m.NewContext(ctx)
		defer func() { err = errors.Join(err, metrics.Flush(ctx)) }()
		return metrics.AddMetric(ctx, "SuccessfulBooking", metrics.Count, 1)
	}

	if err := work(context.Background()); err != nil {
		panic(err)
	}
	fmt.Print(out.String())
	// Output:
	// {"_aws":{"Timestamp":1700000000000,"CloudWatchMetrics":[{"Namespace":"ServerlessAirline","Dimensions":[[]],"Metrics":[{"Name":"SuccessfulBooking","Unit":"Count"}]}]},"SuccessfulBooking":[1.0]}
}
