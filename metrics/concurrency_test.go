package metrics_test

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
)

// TestConcurrentInvocations runs invocations side by side, as Lambda
// Managed Instances do, each fanning out to goroutines of its own: every
// document must hold the metrics, dimensions and metadata of one invocation
// only, and exactly one invocation reports the cold start.
func TestConcurrentInvocations(t *testing.T) { //nolint:paralleltest // the cold start claim is process-wide
	const (
		invocations = 64
		fanOut      = 8
	)
	coldstart.Reset()
	m, out := newGolden(metrics.WithNamespace("ns"), metrics.WithService("svc"), metrics.WithFunctionName("fn"))
	h := m.LogMetrics(func(ctx context.Context, id int) (int, error) {
		value := strconv.Itoa(id)
		if err := metrics.AddDimension(ctx, "invocation", value); err != nil {
			return 0, err
		}
		if err := metrics.AddMetadata(ctx, "request", value); err != nil {
			return 0, err
		}
		var wg sync.WaitGroup
		errs := make(chan error, fanOut)
		for range fanOut {
			wg.Go(func() {
				errs <- metrics.AddMetric(ctx, "work", metrics.Count, float64(id))
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				return 0, err
			}
		}
		return id, metrics.AddMetric(ctx, "id", metrics.Count, float64(id))
	}, metrics.WithColdStartMetric(true))

	var wg sync.WaitGroup
	results := make([]int, invocations)
	errs := make([]error, invocations)
	for id := range invocations {
		wg.Go(func() {
			ctx := lambdacontext.NewContext(t.Context(), &lambdacontext.LambdaContext{AwsRequestID: "req-" + strconv.Itoa(id)})
			results[id], errs[id] = h(ctx, id)
		})
	}
	wg.Wait()
	for id := range invocations {
		require.NoError(t, errs[id])
		assert.Equal(t, id, results[id])
	}

	docs := documents(t, out)
	require.Len(t, docs, invocations+1)
	coldStarts := 0
	seen := map[string]bool{}
	for _, doc := range docs {
		if _, ok := doc["ColdStart"]; ok {
			coldStarts++
			assert.Equal(t, "fn", doc["function_name"])
			assert.NotContains(t, doc, "invocation")
			continue
		}
		id, ok := doc["invocation"].(string)
		require.True(t, ok)
		assert.False(t, seen[id], "invocation %s flushed twice", id)
		seen[id] = true
		n, err := strconv.Atoi(id)
		require.NoError(t, err)
		assert.Equal(t, id, doc["request"])
		assert.Equal(t, []any{float64(n)}, doc["id"])
		work, ok := doc["work"].([]any)
		require.True(t, ok)
		require.Len(t, work, fanOut)
		for _, v := range work {
			assert.InDelta(t, float64(n), v, 0)
		}
	}
	assert.Equal(t, 1, coldStarts)
	assert.Len(t, seen, invocations)
}
