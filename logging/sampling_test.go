package logging

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sequence returns the draws in order, then 0.99 forever.
func sequence(draws ...float64) func() float64 {
	var mu sync.Mutex
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		if len(draws) == 0 {
			return 0.99
		}
		next := draws[0]
		draws = draws[1:]
		return next
	}
}

func TestSamplingIsDrawnPerInvocation(t *testing.T) {
	t.Parallel()
	// The first draw is the logger's own, for records outside invocations.
	f := newFixture(t, WithoutKeys(KeyLocation), WithSamplingRate(0.1), WithRandom(sequence(0.9, 0.05, 0.9, 0.1)))
	for i := range 3 {
		f.invoke(t.Context(), func(ctx context.Context) {
			f.logger.DebugContext(ctx, "debug", "invocation", i)
		})
	}
	assert.Equal(t, []string{
		`{"level":"DEBUG","message":"debug","timestamp":"` + testTimestamp + `","service":"payment","sampling_rate":0.1,"invocation":0}`,
		`{"level":"DEBUG","message":"debug","timestamp":"` + testTimestamp + `","service":"payment","sampling_rate":0.1,"invocation":2}`,
	}, f.out.lines())
}

func TestSamplingOutsideInvocations(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithSamplingRate(0.5), WithRandom(sequence(0.2, 0.7, 0.2)))
	f.logger.DebugContext(t.Context(), "sampled at construction")
	f.logger.RefreshSampleRateCalculation(t.Context())
	f.logger.DebugContext(t.Context(), "not sampled")
	f.invoke(t.Context(), func(ctx context.Context) {
		f.logger.DebugContext(ctx, "this invocation drew 0.2")
		f.logger.RefreshSampleRateCalculation(ctx)
		f.logger.DebugContext(ctx, "redrawn: 0.99")
	})
	assert.Equal(t, []string{"sampled at construction", "this invocation drew 0.2"}, f.messages(t))
}

func TestSamplingRateBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rate    float64
		draw    float64
		sampled bool
	}{
		{rate: 0, draw: 0, sampled: false},
		{rate: -1, draw: 0, sampled: false},
		{rate: 1, draw: 0.999, sampled: true},
		{rate: 2, draw: 0.999, sampled: true},
		{rate: 0.5, draw: 0.5, sampled: true},
		{rate: 0.5, draw: 0.51, sampled: false},
	}
	for _, tc := range cases {
		f := newFixture(t, WithSamplingRate(tc.rate), WithRandom(func() float64 { return tc.draw }))
		assert.Equal(t, tc.sampled, f.logger.Enabled(t.Context(), -4), "rate %v, draw %v", tc.rate, tc.draw)
	}
}

func TestSamplingNeverRaisesTheLevel(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithLevel(LevelTrace), WithSamplingRate(1))
	assert.True(t, f.logger.Enabled(t.Context(), LevelTrace))
}

// TestSamplingStatistics checks that the fraction of sampled invocations is
// the rate, with a seeded source.
func TestSamplingStatistics(t *testing.T) {
	t.Parallel()
	source := rand.New(rand.NewPCG(1, 2))
	var mu sync.Mutex
	random := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return source.Float64()
	}
	f := newFixture(t, WithoutKeys(KeyLocation, KeyTimestamp, KeyService), WithSamplingRate(0.25), WithRandom(random))
	const invocations = 10000
	for range invocations {
		f.invoke(t.Context(), func(ctx context.Context) { f.logger.DebugContext(ctx, "d") })
	}
	fraction := float64(len(f.out.lines())) / invocations
	assert.InDelta(t, 0.25, fraction, 0.02)
}

func TestSamplingRateFromEnvironment(t *testing.T) {
	t.Setenv("POWERTOOLS_LOGGER_SAMPLE_RATE", "1")
	f := newFixture(t)
	f.invoke(t.Context(), func(ctx context.Context) { f.logger.DebugContext(ctx, "d") })
	records := f.out.records(t)
	require.Len(t, records, 1)
	assert.InDelta(t, 1, records[0][KeySamplingRate], 0)

	f = newFixture(t, WithSamplingRate(0))
	f.invoke(t.Context(), func(ctx context.Context) { f.logger.DebugContext(ctx, "d") })
	assert.Empty(t, f.out.lines(), "the option wins over the variable")

	t.Setenv("POWERTOOLS_LOGGER_SAMPLE_RATE", "often")
	f = newFixture(t)
	f.logger.InfoContext(t.Context(), "hello")
	assert.NotContains(t, f.out.String(), KeySamplingRate, "an invalid rate is ignored")
}

func TestSamplingKeyWithZeroRate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, WithSamplingRate(0))
	f.logger.InfoContext(t.Context(), "hello")
	records := f.out.records(t)
	require.Len(t, records, 1)
	assert.InDelta(t, 0, records[0][KeySamplingRate], 0, "a configured rate shows, even 0, as in Python")
}
