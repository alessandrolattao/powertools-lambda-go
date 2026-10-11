package logging

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentInvocations runs many invocations at once in one process, as
// Lambda Managed Instances do, each with its own keys, correlation id, trace
// id and buffer, and checks that no record carries another invocation's.
func TestConcurrentInvocations(t *testing.T) {
	t.Parallel()
	const invocations = 100
	f := newFixture(t, WithBuffer(BufferMaxBytes(1<<20)))
	handle := f.logger.InjectContext(func(ctx context.Context, i int) (int, error) {
		id := strconv.Itoa(i)
		AppendKeys(ctx, "invocation", id)
		SetCorrelationID(ctx, "corr-"+id)
		scoped := WithKeys(ctx, "scope", id)
		for j := range 5 {
			f.logger.DebugContext(scoped, "debug", "j", j)
			runtime.Gosched()
			f.logger.InfoContext(ctx, "info", "j", j)
		}
		if i%2 == 0 {
			f.logger.ErrorContext(ctx, "failed")
		}
		return i, nil
	})

	results := make([]int, invocations)
	errs := make([]error, invocations)
	var wg sync.WaitGroup
	for i := range invocations {
		wg.Go(func() {
			ctx := withTraceHeader(lambdaContext(t.Context(), fmt.Sprintf("req-%d", i)), traceHeaderFor(i))
			results[i], errs[i] = handle(ctx, i)
		})
	}
	wg.Wait()
	for i := range invocations {
		require.NoError(t, errs[i])
		assert.Equal(t, i, results[i])
	}

	perInvocation := map[string]map[string]int{}
	for _, record := range f.out.records(t) {
		id, ok := record["invocation"].(string)
		require.True(t, ok, "every record carries its invocation: %v", record)
		i, err := strconv.Atoi(id)
		require.NoError(t, err)
		assert.Equal(t, "req-"+id, record[KeyFunctionRequestID])
		assert.Equal(t, "corr-"+id, record[KeyCorrelationID])
		assert.Equal(t, traceIDFor(i), record[KeyXRayTraceID])
		message := fmt.Sprint(record[KeyMessage])
		if message == "debug" {
			assert.Equal(t, id, record["scope"])
		}
		if perInvocation[id] == nil {
			perInvocation[id] = map[string]int{}
		}
		perInvocation[id][message]++
	}
	require.Len(t, perInvocation, invocations)
	for id, counts := range perInvocation {
		i, err := strconv.Atoi(id)
		require.NoError(t, err)
		want := map[string]int{"info": 5}
		if i%2 == 0 {
			want = map[string]int{"debug": 5, "info": 5, "failed": 1}
		}
		assert.Equal(t, want, counts, "invocation %s", id)
	}
}

// TestConcurrentSampling checks that each concurrent invocation keeps the
// sampling decision it drew.
func TestConcurrentSampling(t *testing.T) {
	t.Parallel()
	const invocations = 100
	var draws atomic.Int64
	random := func() float64 {
		// The logger's own draw is the first one: odd draws are kept.
		if draws.Add(1)%2 == 0 {
			return 0
		}
		return 0.99
	}
	f := newFixture(t, WithSamplingRate(0.5), WithRandom(random))
	handle := f.logger.InjectContext(func(ctx context.Context, i int) (int, error) {
		for range 3 {
			f.logger.DebugContext(ctx, "debug", "invocation", i)
			runtime.Gosched()
		}
		return i, nil
	})
	errs := make([]error, invocations)
	var wg sync.WaitGroup
	for i := range invocations {
		wg.Go(func() {
			_, errs[i] = handle(t.Context(), i)
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	counts := map[any]int{}
	for _, record := range f.out.records(t) {
		counts[record["invocation"]]++
	}
	assert.Len(t, counts, invocations/2, "half the invocations are sampled")
	for invocation, n := range counts {
		assert.Equal(t, 3, n, "invocation %v logs all its records or none", invocation)
	}
}

func traceIDFor(i int) string {
	return fmt.Sprintf("1-00000000-%024d", i)
}

func traceHeaderFor(i int) string {
	return "Root=" + traceIDFor(i) + ";Sampled=1"
}
