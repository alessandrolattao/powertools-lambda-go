package idempotency_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: recomputing the keys under test
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

// clock is a time source tests move by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// spyStore counts the calls that reach a Store and can make them fail.
type spyStore struct {
	idempotency.Store

	puts, gets, updates, deletes atomic.Int32
	putErr, getErr               error
	updateErr, deleteErr         error
	deletedWithDoneCtx           atomic.Bool
}

func newSpyStore() *spyStore { return &spyStore{Store: idempotency.NewMemoryStore()} }

func (s *spyStore) PutRecord(ctx context.Context, record idempotency.Record, now time.Time) error {
	s.puts.Add(1)
	if s.putErr != nil {
		return s.putErr
	}
	return s.Store.PutRecord(ctx, record, now)
}

func (s *spyStore) GetRecord(ctx context.Context, key string) (*idempotency.Record, error) {
	s.gets.Add(1)
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.GetRecord(ctx, key)
}

func (s *spyStore) UpdateRecord(ctx context.Context, record idempotency.Record) error {
	s.updates.Add(1)
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.UpdateRecord(ctx, record)
}

func (s *spyStore) DeleteRecord(ctx context.Context, key string) error {
	s.deletes.Add(1)
	s.deletedWithDoneCtx.Store(ctx.Err() != nil)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Store.DeleteRecord(ctx, key)
}

type order struct {
	ID       string  `json:"id"`
	Customer string  `json:"customer"`
	Amount   float64 `json:"amount"`
}

type receipt struct {
	OrderID string `json:"order_id"`
	Run     int32  `json:"run"`
}

// counter is a function that counts its runs.
type counter struct{ runs atomic.Int32 }

func (c *counter) process(_ context.Context, o order) (receipt, error) {
	return receipt{OrderID: o.ID, Run: c.runs.Add(1)}, nil
}

// quiet keeps the warnings out of the test output.
var quiet = idempotency.WithWarnings(slog.New(slog.DiscardHandler))

// withDeadline gives ctx the deadline Lambda gives an invocation.
func withDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func assertSameTime(t *testing.T, want, got time.Time) {
	t.Helper()
	assert.Truef(t, want.Equal(got), "want %v, got %v", want, got)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // G401: recomputing the keys under test
	return hex.EncodeToString(sum[:])
}

func TestReplaysTheFirstResult(t *testing.T) {
	t.Parallel()
	var c counter
	store := newSpyStore()
	process := idempotency.Idempotent(c.process, store, quiet)
	ctx := withDeadline(t)

	first, err := process(ctx, order{ID: "o-1", Amount: 10})
	require.NoError(t, err)
	second, err := process(ctx, order{ID: "o-1", Amount: 10})
	require.NoError(t, err)
	other, err := process(ctx, order{ID: "o-2", Amount: 10})
	require.NoError(t, err)

	assert.Equal(t, receipt{OrderID: "o-1", Run: 1}, first)
	assert.Equal(t, first, second, "the replay returns the stored result")
	assert.Equal(t, receipt{OrderID: "o-2", Run: 2}, other)
	assert.Equal(t, int32(2), c.runs.Load())
	assert.Equal(t, int32(3), store.puts.Load())
	assert.Equal(t, int32(2), store.updates.Load())
}

func TestRecordsAreWrittenAsPythonWritesThem(t *testing.T) {
	t.Parallel()
	clk := newClock()
	store := idempotency.NewMemoryStore()
	var c counter
	process := idempotency.Idempotent(c.process, store,
		idempotency.WithEventKeyJMESPath("[id, customer]"),
		idempotency.WithPayloadValidationJMESPath("amount"),
		idempotency.WithKeyPrefix("orders"),
		idempotency.WithExpiresAfter(90*time.Second+500*time.Millisecond),
		idempotency.WithClock(clk.Now),
	)
	ctx, cancel := context.WithDeadline(t.Context(), clk.Now().Add(3*time.Second+250*time.Millisecond))
	defer cancel()

	_, err := process(ctx, order{ID: "o-1", Customer: "c-1", Amount: 10})
	require.NoError(t, err)

	key := "orders#" + md5Hex(`["o-1", "c-1"]`)
	record, err := store.GetRecord(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, idempotency.StatusCompleted, record.Status)
	assertSameTime(t, clk.Now().Add(90*time.Second), record.ExpiresAt) // whole seconds
	assert.Equal(t, md5Hex("10"), record.PayloadHash, "integral numbers hash as integers")
	assert.JSONEq(t, `{"order_id":"o-1","run":1}`, record.ResponseData)
}

func TestInProgressRecordCarriesTheDeadline(t *testing.T) {
	t.Parallel()
	clk := newClock()
	store := idempotency.NewMemoryStore()
	deadline := clk.Now().Add(3*time.Second + 250*time.Millisecond)
	var seen idempotency.Record
	process := idempotency.Idempotent(func(ctx context.Context, o order) (string, error) {
		record, err := store.GetRecord(ctx, "p#"+md5Hex(`"o-1"`))
		if err != nil {
			return "", err
		}
		seen = *record
		return o.ID, nil
	}, store, idempotency.WithKeyPrefix("p"), idempotency.WithEventKeyJMESPath("id"), idempotency.WithClock(clk.Now))
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()

	_, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, idempotency.StatusInProgress, seen.Status)
	assertSameTime(t, deadline, seen.InProgressExpiresAt)
	assertSameTime(t, clk.Now().Add(time.Hour), seen.ExpiresAt)
}

func TestNoDeadlineWarnsAndLeavesTheLockToTheExpiry(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	store := idempotency.NewMemoryStore()
	var seen idempotency.Record
	process := idempotency.Idempotent(func(ctx context.Context, o order) (string, error) {
		record, err := store.GetRecord(ctx, "p#"+md5Hex(`"o-1"`))
		if err != nil {
			return "", err
		}
		seen = *record
		return o.ID, nil
	}, store, idempotency.WithKeyPrefix("p"), idempotency.WithEventKeyJMESPath("id"),
		idempotency.WithWarnings(slog.New(slog.NewJSONHandler(&logs, nil))))

	for range 2 {
		_, err := process(context.WithoutCancel(t.Context()), order{ID: "o-1"})
		require.NoError(t, err)
	}
	assert.True(t, seen.InProgressExpiresAt.IsZero())
	assert.Equal(t, 1, strings.Count(logs.String(), "the context has no deadline"), "warned once")
}

func TestEventKeyIgnoresTheRestOfThePayload(t *testing.T) {
	t.Parallel()
	var c counter
	process := idempotency.Idempotent(c.process, idempotency.NewMemoryStore(),
		idempotency.WithEventKeyJMESPath("id"), quiet)
	ctx := withDeadline(t)

	first, err := process(ctx, order{ID: "o-1", Amount: 10})
	require.NoError(t, err)
	second, err := process(ctx, order{ID: "o-1", Amount: 99})
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(1), c.runs.Load())
}

func TestPayloadValidation(t *testing.T) {
	t.Parallel()
	var c counter
	process := idempotency.Idempotent(c.process, idempotency.NewMemoryStore(),
		idempotency.WithEventKeyJMESPath("id"),
		idempotency.WithPayloadValidationJMESPath("amount"))
	ctx := withDeadline(t)

	_, err := process(ctx, order{ID: "o-1", Amount: 10})
	require.NoError(t, err)
	_, err = process(ctx, order{ID: "o-1", Amount: 10, Customer: "changed"})
	require.NoError(t, err, "the validated part did not change")
	_, err = process(ctx, order{ID: "o-1", Amount: 11})
	require.ErrorIs(t, err, idempotency.ErrPayloadValidation)
	assert.Equal(t, int32(1), c.runs.Load())
}

func TestPayloadValidationOfCachedRecords(t *testing.T) {
	t.Parallel()
	var c counter
	process := idempotency.Idempotent(c.process, idempotency.NewMemoryStore(),
		idempotency.WithEventKeyJMESPath("id"),
		idempotency.WithPayloadValidationJMESPath("amount"),
		idempotency.WithLocalCache(true))
	ctx := withDeadline(t)

	_, err := process(ctx, order{ID: "o-1", Amount: 10})
	require.NoError(t, err)
	_, err = process(ctx, order{ID: "o-1", Amount: 11})
	require.ErrorIs(t, err, idempotency.ErrPayloadValidation)
}

func TestMissingKey(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	var c counter
	store := newSpyStore()
	process := idempotency.Idempotent(c.process, store,
		idempotency.WithEventKeyJMESPath("customer"),
		idempotency.WithWarnings(slog.New(slog.NewJSONHandler(&logs, nil))))
	ctx := withDeadline(t)

	for range 2 {
		_, err := process(ctx, order{ID: "o-1"})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(2), c.runs.Load(), "without a key, every call runs")
	assert.Zero(t, store.puts.Load(), "and leaves no record")
	assert.Equal(t, 1, strings.Count(logs.String(), "no idempotency key value found"))
	assert.Contains(t, logs.String(), `"jmespath":"customer"`)
}

func TestKeyRequired(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	process := idempotency.Idempotent(func(context.Context, map[string]any) (string, error) {
		runs.Add(1)
		return "done", nil
	}, idempotency.NewMemoryStore(),
		idempotency.WithEventKeyJMESPath("[customer, id]"),
		idempotency.WithKeyRequired(true))

	_, err := process(withDeadline(t), map[string]any{"amount": 1})
	require.ErrorIs(t, err, idempotency.ErrNoIdempotencyKey)
	assert.Zero(t, runs.Load())

	_, err = process(withDeadline(t), map[string]any{"id": "o-1"})
	require.NoError(t, err, "one value is enough")
	_, err = process(withDeadline(t), map[string]any{"id": 0})
	require.NoError(t, err, "zero is a key")
}

func TestFunctionErrorDeletesTheRecord(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	var runs atomic.Int32
	store := newSpyStore()
	process := idempotency.Idempotent(func(context.Context, order) (string, error) {
		if runs.Add(1) == 1 {
			return "partial", boom
		}
		return "done", nil
	}, store, quiet)
	ctx := withDeadline(t)

	got, err := process(ctx, order{ID: "o-1"})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, "partial", got, "the function's own result goes back")
	assert.Equal(t, int32(1), store.deletes.Load())

	got, err = process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "done", got, "a retry runs the function again")
}

func TestCancellationDeletesTheRecord(t *testing.T) {
	t.Parallel()
	store := newSpyStore()
	ctx, cancel := context.WithCancel(withDeadline(t))
	process := idempotency.Idempotent(func(ctx context.Context, _ order) (string, error) {
		cancel()
		return "", ctx.Err()
	}, store, quiet)

	_, err := process(ctx, order{ID: "o-1"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), store.deletes.Load())
	assert.False(t, store.deletedWithDoneCtx.Load(), "the delete outlives the cancellation")
}

func TestDeleteFailureJoinsBothErrors(t *testing.T) {
	t.Parallel()
	boom, broken := errors.New("boom"), errors.New("store down")
	store := newSpyStore()
	store.deleteErr = broken
	process := idempotency.Idempotent(func(context.Context, order) (string, error) {
		return "", boom
	}, store, quiet)

	_, err := process(withDeadline(t), order{ID: "o-1"})
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, err, broken)
	perr, ok := errors.AsType[*idempotency.PersistenceError](err)
	require.True(t, ok)
	assert.Equal(t, idempotency.OpDelete, perr.Op)
	assert.ErrorIs(t, err, idempotency.ErrPersistence)
}

func TestPanicDeletesTheRecordAndGoesOn(t *testing.T) {
	t.Parallel()
	store := newSpyStore()
	var runs atomic.Int32
	process := idempotency.Idempotent(func(context.Context, order) (string, error) {
		if runs.Add(1) == 1 {
			panic("kaboom")
		}
		return "done", nil
	}, store, quiet)
	ctx := withDeadline(t)

	assert.PanicsWithValue(t, "kaboom", func() { _, _ = process(ctx, order{ID: "o-1"}) }) //nolint:errcheck // it panics
	assert.Equal(t, int32(1), store.deletes.Load())

	got, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "done", got)
}

func TestConcurrentCallIsAlreadyInProgress(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	process := idempotency.Idempotent(func(_ context.Context, o order) (string, error) {
		close(started)
		<-release
		return o.ID, nil
	}, idempotency.NewMemoryStore(), idempotency.WithEventKeyJMESPath("id"))
	ctx := withDeadline(t)

	var wg sync.WaitGroup
	var first string
	var firstErr error
	wg.Go(func() { first, firstErr = process(ctx, order{ID: "o-1"}) })
	<-started

	_, err := process(ctx, order{ID: "o-1"})
	require.ErrorIs(t, err, idempotency.ErrAlreadyInProgress)
	inProgress, ok := errors.AsType[*idempotency.AlreadyInProgressError](err)
	require.True(t, ok)
	assert.Equal(t, "id", inProgress.KeyExpression)
	assert.Contains(t, err.Error(), "execution already in progress with idempotency key: id=")

	close(release)
	wg.Wait()
	require.NoError(t, firstErr)
	assert.Equal(t, "o-1", first)

	again, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "o-1", again)
}

func TestTimedOutInProgressRecordIsTakenOver(t *testing.T) {
	t.Parallel()
	clk := newClock()
	store := idempotency.NewMemoryStore()
	var c counter
	process := idempotency.Idempotent(c.process, store,
		idempotency.WithKeyPrefix("p"), idempotency.WithEventKeyJMESPath("id"), idempotency.WithClock(clk.Now))
	key := "p#" + md5Hex(`"o-1"`)
	// An invocation that timed out a second ago left its record behind.
	require.NoError(t, store.PutRecord(t.Context(), idempotency.Record{
		Key:                 key,
		Status:              idempotency.StatusInProgress,
		ExpiresAt:           clk.Now().Add(time.Hour),
		InProgressExpiresAt: clk.Now().Add(-time.Second),
	}, clk.Now()))

	got, err := process(withDeadline(t), order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), got.Run)
}

func TestInProgressRecordWithoutDeadlineLocksUntilExpiry(t *testing.T) {
	t.Parallel()
	clk := newClock()
	store := idempotency.NewMemoryStore()
	var c counter
	process := idempotency.Idempotent(c.process, store,
		idempotency.WithKeyPrefix("p"), idempotency.WithEventKeyJMESPath("id"), idempotency.WithClock(clk.Now))
	require.NoError(t, store.PutRecord(t.Context(), idempotency.Record{
		Key:       "p#" + md5Hex(`"o-1"`),
		Status:    idempotency.StatusInProgress,
		ExpiresAt: clk.Now().Add(time.Minute),
	}, clk.Now()))

	_, err := process(withDeadline(t), order{ID: "o-1"})
	require.ErrorIs(t, err, idempotency.ErrAlreadyInProgress)

	clk.Advance(time.Minute + time.Second)
	_, err = process(withDeadline(t), order{ID: "o-1"})
	require.NoError(t, err)
}

func TestExpiredRecordRunsAgain(t *testing.T) {
	t.Parallel()
	clk := newClock()
	var c counter
	process := idempotency.Idempotent(c.process, idempotency.NewMemoryStore(),
		idempotency.WithExpiresAfter(10*time.Second), idempotency.WithClock(clk.Now), idempotency.WithLocalCache(true))
	ctx := withDeadline(t)

	_, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	clk.Advance(10 * time.Second)
	got, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), got.Run, "still within the second it expires in")

	clk.Advance(time.Second)
	got, err = process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), got.Run)
}

func TestLocalCacheSkipsTheStore(t *testing.T) {
	t.Parallel()
	var c counter
	store := newSpyStore()
	process := idempotency.Idempotent(c.process, store, idempotency.WithLocalCache(true), quiet)
	ctx := withDeadline(t)

	for range 3 {
		_, err := process(ctx, order{ID: "o-1"})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), c.runs.Load())
	assert.Equal(t, int32(1), store.puts.Load(), "replays come from the cache")
	assert.Zero(t, store.gets.Load())
}

func TestReplayCachesTheRecordItRead(t *testing.T) {
	t.Parallel()
	var c counter
	store := newSpyStore()
	ctx := withDeadline(t)
	// Another execution environment completed the record.
	_, err := idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("p"), quiet)(ctx, order{ID: "o-1"})
	require.NoError(t, err)

	process := idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("p"), idempotency.WithLocalCache(true), quiet)
	for range 3 {
		_, err := process(ctx, order{ID: "o-1"})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), c.runs.Load())
	assert.Equal(t, int32(2), store.puts.Load(), "one write to find the record, then the cache")
}

func TestLocalCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	var c counter
	store := newSpyStore()
	process := idempotency.Idempotent(c.process, store,
		idempotency.WithLocalCache(true), idempotency.WithLocalCacheMaxItems(2), quiet)
	ctx := withDeadline(t)

	for _, id := range []string{"a", "b", "a", "c", "a", "b"} {
		_, err := process(ctx, order{ID: id})
		require.NoError(t, err)
	}
	// a, b and c are written; the second a hits the cache; c evicts b, the
	// least recently used; the third a hits; b goes back to the store.
	assert.Equal(t, int32(4), store.puts.Load())
	assert.Equal(t, int32(3), c.runs.Load())
}

func TestResponseHook(t *testing.T) {
	t.Parallel()
	var c counter
	var hooked []idempotency.Record
	process := idempotency.Idempotent(c.process, idempotency.NewMemoryStore(),
		idempotency.WithResponseHook(func(_ context.Context, r receipt, record idempotency.Record) (receipt, error) {
			hooked = append(hooked, record)
			r.OrderID += " (replay)"
			return r, nil
		}), quiet)
	ctx := withDeadline(t)

	first, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "o-1", first.OrderID, "not on the first execution")
	replay, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, "o-1 (replay)", replay.OrderID)
	require.Len(t, hooked, 1)
	assert.Equal(t, idempotency.StatusCompleted, hooked[0].Status)
}

func TestResponseHookError(t *testing.T) {
	t.Parallel()
	hookErr := errors.New("hook")
	var c counter
	process := idempotency.Idempotent(c.process, idempotency.NewMemoryStore(),
		idempotency.WithResponseHook(func(context.Context, receipt, idempotency.Record) (receipt, error) {
			return receipt{}, hookErr
		}), quiet)
	ctx := withDeadline(t)

	_, err := process(ctx, order{ID: "o-1"})
	require.NoError(t, err)
	_, err = process(ctx, order{ID: "o-1"})
	require.ErrorIs(t, err, hookErr)
}

// TestFalsyResponsesReplay fixes Python's replay of {}, [], 0, "" and false
// as None.
func TestFalsyResponsesReplay(t *testing.T) {
	t.Parallel()
	ctx := withDeadline(t)
	var runs atomic.Int32
	zero := idempotency.Idempotent(func(context.Context, order) (map[string]int, error) {
		runs.Add(1)
		return map[string]int{}, nil
	}, idempotency.NewMemoryStore(), quiet)
	for range 2 {
		got, err := zero(ctx, order{ID: "o-1"})
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	}
	assert.Equal(t, int32(1), runs.Load())
}

func TestDisabledByEnvironment(t *testing.T) {
	t.Setenv("POWERTOOLS_IDEMPOTENCY_DISABLED", "true")
	var logs bytes.Buffer
	var c counter
	store := newSpyStore()
	process := idempotency.Idempotent(c.process, store,
		idempotency.WithWarnings(slog.New(slog.NewJSONHandler(&logs, nil))))

	for range 2 {
		_, err := process(withDeadline(t), order{ID: "o-1"})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(2), c.runs.Load())
	assert.Zero(t, store.puts.Load())
	assert.Equal(t, 1, strings.Count(logs.String(), "idempotency is disabled"))
}

func TestDefaultKeyPrefix(t *testing.T) {
	const function = ".github.com/alessandrolattao/powertools-lambda-go/idempotency_test.(*counter).process-fm#"
	hash := md5Hex(`{"amount": 0, "customer": "", "id": "o-1"}`)
	assertKey := func(key string) {
		t.Helper()
		store := idempotency.NewMemoryStore()
		var c counter
		_, err := idempotency.Idempotent(c.process, store)(withDeadline(t), order{ID: "o-1"})
		require.NoError(t, err)
		_, err = store.GetRecord(t.Context(), key)
		require.NoError(t, err, key)
	}

	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "orders-fn")
	assertKey("orders-fn" + function + hash)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "")
	assertKey("test-func" + function + hash)
}

func TestHashFunction(t *testing.T) {
	t.Parallel()
	store := idempotency.NewMemoryStore()
	var c counter
	_, err := idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("p"),
		idempotency.WithEventKeyJMESPath("id"), idempotency.WithHashFunction("sha256"))(withDeadline(t), order{ID: "o-1"})
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(`"o-1"`))
	_, err = store.GetRecord(t.Context(), "p#"+hex.EncodeToString(sum[:]))
	require.NoError(t, err)

	_, err = idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("q"),
		idempotency.WithEventKeyJMESPath("id"), idempotency.WithHash(sha256.New))(withDeadline(t), order{ID: "o-1"})
	require.NoError(t, err)
	_, err = store.GetRecord(t.Context(), "q#"+hex.EncodeToString(sum[:]))
	require.NoError(t, err)
}

func TestCustomJMESPathFunctions(t *testing.T) {
	t.Parallel()
	store := idempotency.NewMemoryStore()
	var c counter
	lower := jmespath.Function{
		Name: "lower",
		Args: []jmespath.Arg{{Types: []jmespath.Type{jmespath.TypeString}}},
		Call: func(args []any) (any, error) {
			s, _ := args[0].(string) //nolint:errcheck // the signature guarantees a string
			return strings.ToLower(s), nil
		},
	}
	process := idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("p"),
		idempotency.WithEventKeyJMESPath("lower(id)"), idempotency.WithJMESPathFunctions(lower))
	for _, id := range []string{"ABC", "abc"} {
		_, err := process(withDeadline(t), order{ID: id})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), c.runs.Load())
	_, err := store.GetRecord(t.Context(), "p#"+md5Hex(`"abc"`))
	require.NoError(t, err)
}

func TestInvalidConfigPanics(t *testing.T) {
	t.Parallel()
	var c counter
	store := idempotency.NewMemoryStore()
	cases := map[string][]idempotency.Option{
		"expression":       {idempotency.WithEventKeyJMESPath("[")},
		"validation":       {idempotency.WithPayloadValidationJMESPath("a.(")},
		"hash":             {idempotency.WithHashFunction("crc32")},
		"expiry":           {idempotency.WithExpiresAfter(500 * time.Millisecond)},
		"cache size":       {idempotency.WithLocalCache(true), idempotency.WithLocalCacheMaxItems(0)},
		"hook result type": {idempotency.WithResponseHook(func(_ context.Context, s string, _ idempotency.Record) (string, error) { return s, nil })},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				err, ok := recover().(error)
				require.True(t, ok)
				assert.ErrorIs(t, err, idempotency.ErrInvalidConfig)
			}()
			idempotency.Idempotent(c.process, store, opts...)
		})
	}
	assert.Panics(t, func() { idempotency.Idempotent(c.process, nil) })
	assert.Panics(t, func() { idempotency.Idempotent[order, receipt](nil, store) })
}

func TestStoreFailures(t *testing.T) {
	t.Parallel()
	broken := errors.New("store down")
	cases := []struct {
		name  string
		setup func(*spyStore)
		op    string
	}{
		{"put", func(s *spyStore) { s.putErr = broken }, idempotency.OpPut},
		{"update", func(s *spyStore) { s.updateErr = broken }, idempotency.OpUpdate},
		{"get", func(s *spyStore) { s.putErr, s.getErr = idempotency.ErrItemAlreadyExists, broken }, idempotency.OpGet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newSpyStore()
			tc.setup(store)
			var c counter
			got, err := idempotency.Idempotent(c.process, store, quiet)(withDeadline(t), order{ID: "o-1"})
			require.ErrorIs(t, err, broken)
			perr, ok := errors.AsType[*idempotency.PersistenceError](err)
			require.True(t, ok)
			assert.Equal(t, tc.op, perr.Op)
			if tc.op == idempotency.OpUpdate {
				assert.Equal(t, "o-1", got.OrderID, "the function ran: its result goes back with the error")
			}
		})
	}
}

func TestPersistenceErrorMessages(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	for op, want := range map[string]string{
		idempotency.OpPut:    "failed to save in progress record to idempotency store: cause",
		idempotency.OpGet:    "failed to get record from idempotency store: cause",
		idempotency.OpUpdate: "failed to update record state to success in idempotency store: cause",
		idempotency.OpDelete: "failed to delete record from idempotency store: cause",
	} {
		assert.EqualError(t, &idempotency.PersistenceError{Op: op, Err: cause}, want)
	}
}

// vanishingStore reports a record on every write and finds none on every
// read: a record deleted between the two, over and over.
type vanishingStore struct {
	spyStore
}

func (s *vanishingStore) PutRecord(context.Context, idempotency.Record, time.Time) error {
	s.puts.Add(1)
	return idempotency.ErrItemAlreadyExists
}

func (s *vanishingStore) GetRecord(context.Context, string) (*idempotency.Record, error) {
	s.gets.Add(1)
	return nil, idempotency.ErrItemNotFound
}

func TestInconsistentStateRetries(t *testing.T) {
	t.Parallel()
	store := &vanishingStore{}
	var c counter
	_, err := idempotency.Idempotent(c.process, store, quiet)(withDeadline(t), order{ID: "o-1"})
	require.ErrorIs(t, err, idempotency.ErrInconsistentState)
	assert.Equal(t, int32(3), store.puts.Load(), "three attempts")
	assert.Equal(t, int32(3), store.gets.Load())
	assert.Zero(t, c.runs.Load())
}

// TestNestedInconsistentStateIsNotRetried checks that the retries only
// follow the store: an error the function returns runs it once, whatever it
// wraps.
func TestNestedInconsistentStateIsNotRetried(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	_, err := idempotency.Idempotent(func(context.Context, order) (string, error) {
		runs.Add(1)
		return "", idempotency.ErrInconsistentState
	}, idempotency.NewMemoryStore(), quiet)(withDeadline(t), order{ID: "o-1"})
	require.ErrorIs(t, err, idempotency.ErrInconsistentState)
	assert.Equal(t, int32(1), runs.Load())
}

// staleStore answers the first write with a record that expired after the
// check; the second attempt goes through.
type staleStore struct {
	*idempotency.MemoryStore

	calls atomic.Int32
}

func (s *staleStore) PutRecord(ctx context.Context, record idempotency.Record, now time.Time) error {
	if s.calls.Add(1) == 1 {
		old := record
		old.Status = idempotency.StatusCompleted
		old.ExpiresAt = now.Add(-time.Minute)
		return &idempotency.ItemAlreadyExistsError{Old: &old}
	}
	return s.MemoryStore.PutRecord(ctx, record, now)
}

func TestExpiredRecordFromTheStoreIsRetried(t *testing.T) {
	t.Parallel()
	store := &staleStore{MemoryStore: idempotency.NewMemoryStore()}
	var c counter
	got, err := idempotency.Idempotent(c.process, store, quiet)(withDeadline(t), order{ID: "o-1"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), got.Run)
	assert.Equal(t, int32(2), store.calls.Load())
}

func TestInvalidStoredStatus(t *testing.T) {
	t.Parallel()
	clk := newClock()
	store := idempotency.NewMemoryStore()
	require.NoError(t, store.UpdateRecord(t.Context(), idempotency.Record{
		Key: "p#" + md5Hex(`"o-1"`), Status: "DONE", ExpiresAt: clk.Now().Add(time.Hour),
	}))
	var c counter
	_, err := idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("p"),
		idempotency.WithEventKeyJMESPath("id"), idempotency.WithClock(clk.Now))(withDeadline(t), order{ID: "o-1"})
	require.ErrorIs(t, err, idempotency.ErrInvalidStatus)
	assert.EqualError(t, err, `invalid idempotency record status "DONE"`)
}

func TestUndecodableStoredResponse(t *testing.T) {
	t.Parallel()
	clk := newClock()
	store := idempotency.NewMemoryStore()
	require.NoError(t, store.UpdateRecord(t.Context(), idempotency.Record{
		Key: "p#" + md5Hex(`"o-1"`), Status: idempotency.StatusCompleted,
		ExpiresAt: clk.Now().Add(time.Hour), ResponseData: `"a string, not a receipt"`,
	}))
	var c counter
	_, err := idempotency.Idempotent(c.process, store, idempotency.WithKeyPrefix("p"),
		idempotency.WithEventKeyJMESPath("id"), idempotency.WithClock(clk.Now))(withDeadline(t), order{ID: "o-1"})
	require.ErrorContains(t, err, "decode the stored response")
	assert.Zero(t, c.runs.Load())
}

func TestUnmarshallableResponse(t *testing.T) {
	t.Parallel()
	store := newSpyStore()
	got, err := idempotency.Idempotent(func(context.Context, order) (chan int, error) {
		return make(chan int), nil
	}, store, quiet)(withDeadline(t), order{ID: "o-1"})
	require.ErrorIs(t, err, idempotency.ErrPersistence)
	assert.NotNil(t, got)
	assert.Zero(t, store.updates.Load())
}

func TestUnmarshallablePayload(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	_, err := idempotency.Idempotent(func(context.Context, chan int) (string, error) {
		runs.Add(1)
		return "", nil
	}, idempotency.NewMemoryStore(), quiet)(withDeadline(t), make(chan int))
	require.ErrorContains(t, err, "marshal the idempotency payload")
	assert.Zero(t, runs.Load())
}

func TestEnvelopeErrorInTheKeyExpression(t *testing.T) {
	t.Parallel()
	type event struct {
		Body string `json:"body"`
	}
	_, err := idempotency.Idempotent(func(context.Context, event) (string, error) {
		return "", nil
	}, idempotency.NewMemoryStore(), idempotency.WithEventKeyJMESPath(jmespath.APIGatewayREST))(withDeadline(t), event{Body: "not json"})
	require.ErrorIs(t, err, jmespath.ErrInvalidEnvelope)
}

// TestConcurrentInvocations runs many invocations with a few keys at once:
// under the race detector it checks that the shared state is guarded, and
// that a key's function never runs twice at the same time.
func TestConcurrentInvocations(t *testing.T) {
	t.Parallel()
	var running sync.Map
	var overlaps atomic.Int32
	process := idempotency.Idempotent(func(_ context.Context, o order) (string, error) {
		if _, busy := running.LoadOrStore(o.ID, true); busy {
			overlaps.Add(1)
		}
		time.Sleep(time.Millisecond)
		running.Delete(o.ID)
		return o.ID, nil
	}, idempotency.NewMemoryStore(), idempotency.WithEventKeyJMESPath("id"), idempotency.WithLocalCache(true))
	ctx := withDeadline(t)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			id := string(rune('a' + i%4))
			got, err := process(ctx, order{ID: id})
			if err == nil {
				assert.Equal(t, id, got)
			} else {
				assert.ErrorIs(t, err, idempotency.ErrAlreadyInProgress)
			}
		})
	}
	wg.Wait()
	assert.Zero(t, overlaps.Load())
}
