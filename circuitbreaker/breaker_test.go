package circuitbreaker_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cb "github.com/alessandrolattao/powertools-lambda-go/circuitbreaker"
)

var (
	errDown     = errors.New("downstream unavailable")
	errBadInput = errors.New("bad input")
)

func succeed(context.Context) (string, error) { return "ok", nil }
func fail(context.Context) (string, error)    { return "", errDown }

// harness is a breaker on a memory store with a clock the test moves.
type harness struct {
	store       *memStore
	clock       *clock
	breaker     *cb.Breaker
	mu          sync.Mutex
	transitions []cb.Transition
}

func newHarness(t *testing.T, opts ...cb.Option) *harness {
	t.Helper()
	h := &harness{store: newMemStore(), clock: newClock()}
	h.breaker = h.add(t, opts...)
	return h
}

// add creates another breaker on the same circuit and store: another
// execution environment.
func (h *harness) add(t *testing.T, opts ...cb.Option) *cb.Breaker {
	t.Helper()
	opts = append([]cb.Option{
		cb.WithFailureThreshold(3),
		cb.WithRecoveryTimeout(10 * time.Second),
		cb.WithSuccessThreshold(2),
		cb.WithLocalCacheMaxAge(0),
		cb.WithOnTransition(func(_ context.Context, tr cb.Transition) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.transitions = append(h.transitions, tr)
		}),
	}, opts...)
	b, err := cb.New("payments", h.store, opts...)
	require.NoError(t, err)
	cb.SetClock(b, h.clock.Now)
	return b
}

func (h *harness) moves() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.transitions))
	for _, tr := range h.transitions {
		out = append(out, string(tr.From)+">"+string(tr.To))
	}
	return out
}

func (h *harness) trip(t *testing.T) {
	t.Helper()
	for range 3 {
		_, err := h.breaker.Call(t.Context(), fail)
		require.ErrorIs(t, err, errDown)
	}
	require.Equal(t, cb.Open, h.store.record("payments").State)
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	cases := []struct {
		name  string
		cname string
		store cb.Store
		opts  []cb.Option
		ok    bool
	}{
		{name: "defaults", cname: "c", store: store, ok: true},
		{name: "no name", store: store},
		{name: "no store", cname: "c"},
		{name: "zero failure threshold", cname: "c", store: store, opts: []cb.Option{cb.WithFailureThreshold(0)}},
		{name: "negative success threshold", cname: "c", store: store, opts: []cb.Option{cb.WithSuccessThreshold(-1)}},
		{name: "zero recovery timeout", cname: "c", store: store, opts: []cb.Option{cb.WithRecoveryTimeout(0)}},
		{name: "fractional recovery timeout", cname: "c", store: store, opts: []cb.Option{cb.WithRecoveryTimeout(1500 * time.Millisecond)}},
		{name: "negative cache age", cname: "c", store: store, opts: []cb.Option{cb.WithLocalCacheMaxAge(-time.Second)}},
		{name: "zero cache age", cname: "c", store: store, opts: []cb.Option{cb.WithLocalCacheMaxAge(0)}, ok: true},
		{name: "empty handled list", cname: "c", store: store, opts: []cb.Option{cb.WithHandledErrors()}},
		{name: "nil ignored error", cname: "c", store: store, opts: []cb.Option{cb.WithIgnoredErrors(errDown, nil)}},
		{name: "nil filter", cname: "c", store: store, opts: []cb.Option{cb.WithFailureFilter(nil)}},
		{
			name: "handled and ignored", cname: "c", store: store,
			opts: []cb.Option{cb.WithHandledErrors(errDown), cb.WithIgnoredErrors(errBadInput)},
		},
		{name: "handled", cname: "c", store: store, opts: []cb.Option{cb.WithHandledErrors(errDown)}, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := cb.New(tc.cname, tc.store, tc.opts...)
			if tc.ok {
				require.NoError(t, err)
				assert.NotNil(t, b)
				return
			}
			require.ErrorIs(t, err, cb.ErrInvalidConfig)
			assert.Nil(t, b)
		})
	}
}

func TestClosedToOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	for range 2 {
		_, err := h.breaker.Call(ctx, fail)
		require.ErrorIs(t, err, errDown)
	}
	assert.Empty(t, h.store.record("payments").State, "a healthy or failing closed circuit never writes")

	// A success resets the streak.
	got, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
	for range 2 {
		_, err = h.breaker.Call(ctx, fail)
		require.ErrorIs(t, err, errDown)
	}
	assert.Empty(t, h.store.record("payments").State)

	// The third consecutive failure trips the circuit, and still returns the
	// downstream's error.
	_, err = h.breaker.Call(ctx, fail)
	require.ErrorIs(t, err, errDown)
	now := h.clock.Now()
	assert.Equal(t, cb.Record{
		Name:         "payments",
		State:        cb.Open,
		FailureCount: 3,
		OpenedAt:     now,
		Expiration:   now.Add(10*time.Second + time.Hour),
	}, h.store.record("payments"))
	assert.Equal(t, []string{"CLOSED>OPEN"}, h.moves())

	// Now calls are rejected without running.
	ran := false
	_, err = h.breaker.Call(ctx, func(context.Context) (string, error) {
		ran = true
		return "", nil
	})
	require.ErrorIs(t, err, cb.ErrCircuitOpen)
	assert.False(t, ran)
	openErr, ok := errors.AsType[*cb.OpenError](err)
	require.True(t, ok)
	assert.Equal(t, cb.CircuitInfo{Name: "payments", State: cb.Open, FailureCount: 3, OpenedAt: now}, openErr.Circuit)
	assert.Equal(t, `circuit "payments" is open`, err.Error())
}

func TestRecovery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	h.trip(t)
	openedAt := h.clock.Now()

	h.clock.Advance(9 * time.Second)
	_, err := h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen, "the recovery timeout has not elapsed")

	h.clock.Advance(time.Second)
	got, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
	record := h.store.record("payments")
	assert.Equal(t, cb.HalfOpen, record.State)
	assert.Equal(t, cb.Owner(h.breaker), record.HalfOpenOwner)
	assert.Equal(t, openedAt, record.OpenedAt)
	assert.Equal(t, h.clock.Now().Add(10*time.Second), record.ProbeLeaseExpiry)
	assert.Equal(t, 0, record.FailureCount)

	// The second successful probe closes it.
	_, err = h.breaker.Call(ctx, succeed)
	require.NoError(t, err)
	record = h.store.record("payments")
	assert.Equal(t, cb.Closed, record.State)
	assert.True(t, record.OpenedAt.IsZero())
	assert.Empty(t, record.HalfOpenOwner)
	assert.True(t, record.ProbeLeaseExpiry.IsZero())
	assert.Equal(t, []string{"CLOSED>OPEN", "OPEN>HALF_OPEN", "HALF_OPEN>CLOSED"}, h.moves())

	h.mu.Lock()
	assert.Equal(t, openedAt, h.transitions[1].OpenedAt)
	assert.True(t, h.transitions[2].OpenedAt.IsZero())
	h.mu.Unlock()

	// Closed again: failures count from zero.
	for range 2 {
		_, err = h.breaker.Call(ctx, fail)
		require.ErrorIs(t, err, errDown)
	}
	assert.Equal(t, cb.Closed, h.store.record("payments").State)
}

func TestProbeFailureReopens(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	h.trip(t)
	h.clock.Advance(10 * time.Second)

	_, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err)
	h.clock.Advance(2 * time.Second)
	_, err = h.breaker.Call(ctx, fail)
	require.ErrorIs(t, err, errDown)

	record := h.store.record("payments")
	assert.Equal(t, cb.Open, record.State)
	assert.Equal(t, h.clock.Now(), record.OpenedAt, "the recovery timeout starts over")
	assert.Empty(t, record.HalfOpenOwner)
	assert.Equal(t, []string{"CLOSED>OPEN", "OPEN>HALF_OPEN", "HALF_OPEN>OPEN"}, h.moves())

	_, err = h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen)

	// The successes before the failure do not count towards closing.
	h.clock.Advance(10 * time.Second)
	_, err = h.breaker.Call(ctx, succeed)
	require.NoError(t, err)
	assert.Equal(t, cb.HalfOpen, h.store.record("payments").State)
}

func TestOtherEnvironmentWaitsForTheProbe(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other := h.add(t)
	ctx := t.Context()
	h.trip(t)

	_, err := other.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen, "the state is shared")

	h.clock.Advance(10 * time.Second)
	_, err = h.breaker.Call(ctx, succeed)
	require.NoError(t, err, "the first breaker wins the probe")

	_, err = other.Call(ctx, succeed)
	openErr, ok := errors.AsType[*cb.OpenError](err)
	require.True(t, ok, "the other one waits: %v", err)
	assert.Equal(t, cb.HalfOpen, openErr.Circuit.State)

	// The prober goes away: once its lease ends, the other takes over,
	// without a transition.
	h.clock.Advance(10 * time.Second)
	_, err = other.Call(ctx, succeed)
	require.NoError(t, err)
	assert.Equal(t, cb.Owner(other), h.store.record("payments").HalfOpenOwner)
	assert.Equal(t, []string{"CLOSED>OPEN", "OPEN>HALF_OPEN"}, h.moves())

	_, err = other.Call(ctx, succeed)
	require.NoError(t, err)
	assert.Equal(t, cb.Closed, h.store.record("payments").State)
}

func TestStaleReadCannotWinTheElection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cb.WithLocalCacheMaxAge(time.Minute))
	ctx := t.Context()
	h.trip(t)
	h.clock.Advance(10 * time.Second)

	// Meanwhile another environment probed, failed and reopened the
	// circuit: this breaker's cached Open record is stale.
	h.store.set(cb.Record{Name: "payments", State: cb.Open, OpenedAt: h.clock.Now()})

	_, err := h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen)
	assert.Equal(t, cb.Open, h.store.record("payments").State)
}

func TestFailureClassification(t *testing.T) {
	t.Parallel()
	errTimeout := errors.New("timeout")
	cases := []struct {
		name   string
		opt    cb.Option
		counts error
		passes error
	}{
		{name: "handled", opt: cb.WithHandledErrors(errTimeout, errDown), counts: errDown, passes: errBadInput},
		{name: "ignored", opt: cb.WithIgnoredErrors(errBadInput), counts: errDown, passes: errBadInput},
		{
			name:   "filter",
			opt:    cb.WithFailureFilter(func(err error) bool { return !errors.Is(err, errBadInput) }),
			counts: errDown,
			passes: errBadInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.opt)
			ctx := t.Context()
			passing := func(context.Context) (string, error) { return "", tc.passes }
			counting := func(context.Context) (string, error) { return "", tc.counts }
			for range 5 {
				_, err := h.breaker.Call(ctx, passing)
				require.ErrorIs(t, err, tc.passes)
			}
			assert.Empty(t, h.store.record("payments").State, "errors that do not count never trip")

			for range 2 {
				_, err := h.breaker.Call(ctx, counting)
				require.ErrorIs(t, err, tc.counts)
			}
			// An error that does not count leaves the streak alone.
			_, err := h.breaker.Call(ctx, passing)
			require.ErrorIs(t, err, tc.passes)
			_, err = h.breaker.Call(ctx, counting)
			require.ErrorIs(t, err, tc.counts)
			assert.Equal(t, cb.Open, h.store.record("payments").State)

			// During a probe, an error that does not count changes nothing.
			h.clock.Advance(10 * time.Second)
			_, err = h.breaker.Call(ctx, passing)
			require.ErrorIs(t, err, tc.passes)
			assert.Equal(t, cb.HalfOpen, h.store.record("payments").State)
			_, err = h.breaker.Call(ctx, succeed)
			require.NoError(t, err, "the breaker still owns the probe")
		})
	}
}

func TestWrap(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	type order struct{ ID string }
	charge := func(_ context.Context, o order) (string, error) {
		if o.ID == "" {
			return "", errDown
		}
		return "charged " + o.ID, nil
	}
	var fallbackCircuit cb.CircuitInfo
	guarded := h.breaker.Wrap(charge, cb.OnCircuitOpen(func(_ context.Context, o order, circuit cb.CircuitInfo) (string, error) {
		fallbackCircuit = circuit
		return "buffered " + o.ID, nil
	}))
	bare := h.breaker.Wrap(charge)

	got, err := guarded(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "charged o-1", got)

	for range 3 {
		_, err = guarded(ctx, order{})
		require.ErrorIs(t, err, errDown)
	}
	got, err = guarded(ctx, order{ID: "o-2"})
	require.NoError(t, err)
	assert.Equal(t, "buffered o-2", got)
	assert.Equal(t, cb.Open, fallbackCircuit.State)
	assert.Equal(t, 3, fallbackCircuit.FailureCount)

	_, err = bare(ctx, order{ID: "o-3"})
	require.ErrorIs(t, err, cb.ErrCircuitOpen)
}

func TestFailsOpenWhenTheStoreIsDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cb.WithLocalCacheMaxAge(time.Minute))
	ctx := t.Context()
	h.store.set(cb.Record{Name: "payments", State: cb.Open, OpenedAt: h.clock.Now()})
	h.store.fail(errors.New("throttled"), nil, nil, nil)

	got, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err, "an unreadable store means a closed circuit")
	assert.Equal(t, "ok", got)

	h.store.fail(nil, nil, nil, nil)
	_, err = h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen, "the guess was not cached")
}

func TestUnknownStateFailsOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.set(cb.Record{Name: "payments", State: "BROKEN"})
	_, err := h.breaker.Call(t.Context(), succeed)
	require.NoError(t, err)
}

func TestWriteFailuresKeepTheResult(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	h.store.fail(nil, errors.New("put failed"), nil, nil)
	for range 3 {
		_, err := h.breaker.Call(ctx, fail)
		require.ErrorIs(t, err, errDown)
		require.NotContains(t, err.Error(), "put failed")
	}
	assert.Equal(t, []string{"CLOSED>OPEN"}, h.moves(), "the hook still fires")
	assert.Empty(t, h.store.record("payments").State)
	_, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err, "the transition is delayed: the store still says closed")
}

func TestCloseWriteFailureKeepsProbing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	h.trip(t)
	h.clock.Advance(10 * time.Second)
	_, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err)

	h.store.fail(nil, nil, nil, errors.New("update failed"))
	_, err = h.breaker.Call(ctx, succeed)
	require.NoError(t, err, "the probe's result is kept")
	assert.Equal(t, cb.HalfOpen, h.store.record("payments").State)
	assert.Equal(t, []string{"CLOSED>OPEN", "OPEN>HALF_OPEN", "HALF_OPEN>CLOSED"}, h.moves())

	// The store still says this breaker probes: it starts counting again.
	h.store.fail(nil, nil, nil, nil)
	for range 2 {
		_, err = h.breaker.Call(ctx, succeed)
		require.NoError(t, err)
	}
	assert.Equal(t, cb.Closed, h.store.record("payments").State)
}

func TestElectionErrorsPropagate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	h.trip(t)
	h.clock.Advance(10 * time.Second)
	errStore := errors.New("store unreachable")
	h.store.fail(nil, nil, errStore, nil)
	_, err := h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, errStore)

	h.store.fail(nil, nil, nil, nil)
	_, err = h.breaker.Call(ctx, succeed)
	require.NoError(t, err, "a failed election leaves the probe free")
}

func TestStaleStreakIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()

	// Two failures here, then the circuit opens and closes elsewhere.
	for range 2 {
		_, err := h.breaker.Call(ctx, fail)
		require.ErrorIs(t, err, errDown)
	}
	h.store.set(cb.Record{Name: "payments", State: cb.Open, OpenedAt: h.clock.Now()})
	_, err := h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen)
	h.store.set(cb.Record{Name: "payments", State: cb.Closed})

	// One more failure does not trip: the old streak is gone.
	_, err = h.breaker.Call(ctx, fail)
	require.ErrorIs(t, err, errDown)
	assert.Equal(t, cb.Closed, h.store.record("payments").State)
}

func TestLocalCache(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cb.WithLocalCacheMaxAge(5*time.Second))
	ctx := t.Context()
	for range 3 {
		_, err := h.breaker.Call(ctx, succeed)
		require.NoError(t, err)
	}
	gets, _, _, _ := h.store.calls()
	assert.Equal(t, 1, gets, "a missing record is cached as closed")

	h.clock.Advance(5 * time.Second)
	_, err := h.breaker.Call(ctx, succeed)
	require.NoError(t, err)
	gets, _, _, _ = h.store.calls()
	assert.Equal(t, 2, gets)

	// A write refreshes the cache: no read after tripping.
	h.trip(t)
	_, err = h.breaker.Call(ctx, succeed)
	require.ErrorIs(t, err, cb.ErrCircuitOpen)
	gets, puts, _, _ := h.store.calls()
	assert.Equal(t, 2, gets)
	assert.Equal(t, 1, puts)
}

func TestSingleProberAmongGoroutines(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cb.WithSuccessThreshold(1))
	other := h.add(t, cb.WithSuccessThreshold(1))
	h.trip(t)
	h.clock.Advance(10 * time.Second)

	release := make(chan struct{})
	var running, ran atomic.Int32
	probeFn := func(ctx context.Context) (string, error) {
		ran.Add(1)
		running.Add(1)
		<-release
		running.Add(-1)
		return "ok", ctx.Err()
	}
	var wg sync.WaitGroup
	var rejected atomic.Int32
	for i := range 40 {
		b := h.breaker
		if i%2 == 1 {
			b = other
		}
		wg.Go(func() {
			_, err := b.Call(t.Context(), probeFn)
			if errors.Is(err, cb.ErrCircuitOpen) {
				rejected.Add(1)
			}
		})
	}
	require.Eventually(t, func() bool { return rejected.Load() == 39 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, int32(1), running.Load())
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), ran.Load())
	assert.Equal(t, cb.Closed, h.store.record("payments").State)
}

func TestCountingIsAtomic(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cb.WithFailureThreshold(10))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, err := h.breaker.Call(t.Context(), fail)
			assert.ErrorIs(t, err, errDown)
		})
	}
	wg.Wait()
	_, puts, _, _ := h.store.calls()
	assert.Equal(t, 1, puts, "exactly one goroutine sees the threshold crossed")
	assert.Equal(t, 10, h.store.record("payments").FailureCount)
}

func TestTransitionHookPanicIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cb.WithOnTransition(func(context.Context, cb.Transition) { panic("metrics down") }))
	for range 3 {
		_, err := h.breaker.Call(t.Context(), fail)
		require.ErrorIs(t, err, errDown)
	}
	assert.Equal(t, cb.Open, h.store.record("payments").State)
}

// ctxStore records whether the writes saw a live context.
type ctxStore struct {
	*memStore

	putCtxErr error
}

func (s *ctxStore) Put(ctx context.Context, record cb.Record) error {
	s.putCtxErr = ctx.Err()
	return s.memStore.Put(ctx, record)
}

func TestWritesOutliveTheCallContext(t *testing.T) {
	t.Parallel()
	store := &ctxStore{memStore: newMemStore()}
	b, err := cb.New("orders", store, cb.WithFailureThreshold(1))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	_, err = b.Call(ctx, func(ctx context.Context) (string, error) {
		cancel()
		return "", ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, store.putCtxErr)
	assert.Equal(t, cb.Open, store.record("orders").State)
}

func TestDisabled(t *testing.T) {
	t.Setenv("POWERTOOLS_CIRCUIT_BREAKER_DISABLED", "true")
	h := newHarness(t)
	h.store.set(cb.Record{Name: "payments", State: cb.Open, OpenedAt: h.clock.Now()})
	for range 5 {
		_, err := h.breaker.Call(t.Context(), fail)
		require.ErrorIs(t, err, errDown)
	}
	got, err := h.breaker.Call(t.Context(), succeed)
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
	gets, puts, acquires, updates := h.store.calls()
	assert.Zero(t, gets+puts+acquires+updates, "the store is never touched")
}
