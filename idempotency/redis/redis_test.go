package redis_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/redis"
)

// newStore starts an in-process Redis and returns a store on it.
func newStore(t *testing.T, opts ...redis.Option) (*redis.Store, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() }) //nolint:errcheck // closing a test client
	store, err := redis.New(client, opts...)
	require.NoError(t, err)
	return store, server
}

func inProgress(now time.Time) idempotency.Record {
	return idempotency.Record{
		Key:                 "fn#h",
		Status:              idempotency.StatusInProgress,
		ExpiresAt:           time.Unix(now.Unix()+3600, 0),
		InProgressExpiresAt: now.Add(30 * time.Second),
		PayloadHash:         "abc",
	}
}

func completed(now time.Time) idempotency.Record {
	return idempotency.Record{
		Key:          "fn#h",
		Status:       idempotency.StatusCompleted,
		ExpiresAt:    time.Unix(now.Unix()+3600, 0),
		ResponseData: `{"ok":true}`,
		PayloadHash:  "abc",
	}
}

// TestValuesArePythons checks the stored JSON against what Powertools Python
// writes: same fields, order and separators, plus the validation hash of
// completed records that Python forgets.
func TestValuesArePythons(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	now := time.UnixMilli(1791633600500)

	require.NoError(t, store.PutRecord(t.Context(), inProgress(now), now))
	got, err := server.Get("fn#h")
	require.NoError(t, err)
	want := `{"status": "INPROGRESS", "expiration": 1791637200, "in_progress_expiration": 1791633630500, "validation": "abc"}`
	assert.Equal(t, want, got)

	require.NoError(t, store.UpdateRecord(t.Context(), completed(now)))
	got, err = server.Get("fn#h")
	require.NoError(t, err)
	want = `{"data": "{\"ok\":true}", "status": "COMPLETED", "expiration": 1791637200, "validation": "abc"}`
	assert.Equal(t, want, got)
}

func TestKeysExpireWithTheRecord(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	now := time.Now()

	require.NoError(t, store.PutRecord(t.Context(), inProgress(now), now))
	assert.InDelta(t, time.Hour.Seconds(), server.TTL("fn#h").Seconds(), 2)

	record := completed(now)
	record.ExpiresAt = now.Add(-time.Minute)
	require.NoError(t, store.UpdateRecord(t.Context(), record))
	assert.Equal(t, time.Second, server.TTL("fn#h"), "never EX 0")
}

func TestGetRecord(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	now := time.UnixMilli(1791633600500)

	_, err := store.GetRecord(t.Context(), "fn#h")
	require.ErrorIs(t, err, idempotency.ErrItemNotFound)
	require.NoError(t, server.Set("fn#h", ""))
	_, err = store.GetRecord(t.Context(), "fn#h")
	require.ErrorIs(t, err, idempotency.ErrItemNotFound)

	require.NoError(t, store.PutRecord(t.Context(), inProgress(now), now))
	got, err := store.GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, "fn#h", got.Key)
	assert.Equal(t, idempotency.StatusInProgress, got.Status)
	assert.Equal(t, int64(1791637200), got.ExpiresAt.Unix())
	assert.Equal(t, int64(1791633630500), got.InProgressExpiresAt.UnixMilli())
	assert.Equal(t, "abc", got.PayloadHash)
}

// TestReadsPythonValues reads a record as Powertools Python writes it.
func TestReadsPythonValues(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	require.NoError(t, server.Set("fn#h", `{"data": "{\"message\": \"success\"}", "status": "COMPLETED", "expiration": 1636549553}`))
	got, err := store.GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, idempotency.Record{
		Key: "fn#h", Status: idempotency.StatusCompleted, ExpiresAt: time.Unix(1636549553, 0),
		ResponseData: `{"message": "success"}`,
	}, *got)
}

func TestPutWhenTheKeyIsHeld(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name  string
		old   func() idempotency.Record
		holds bool
	}{
		{"completed", func() idempotency.Record { return completed(now) }, true},
		{"in progress", func() idempotency.Record { return inProgress(now) }, true},
		{"in progress without deadline", func() idempotency.Record {
			r := inProgress(now)
			r.InProgressExpiresAt = time.Time{}
			return r
		}, true},
		{"timed out", func() idempotency.Record {
			r := inProgress(now)
			r.InProgressExpiresAt = now.Add(-time.Second)
			return r
		}, false},
		{"expired", func() idempotency.Record {
			r := completed(now)
			r.ExpiresAt = now.Add(-2 * time.Second)
			return r
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, server := newStore(t)
			old := tc.old()
			if old.Status == idempotency.StatusCompleted {
				require.NoError(t, store.UpdateRecord(t.Context(), old))
			} else {
				require.NoError(t, store.PutRecord(t.Context(), old, now.Add(-time.Minute)))
			}
			fresh := inProgress(now)
			fresh.PayloadHash = "new"

			err := store.PutRecord(t.Context(), fresh, now)
			if !tc.holds {
				require.NoError(t, err, "an orphan is replaced")
				assert.True(t, server.Exists("fn#h:lock"), "the orphan lock stays")
				got, getErr := store.GetRecord(t.Context(), "fn#h")
				require.NoError(t, getErr)
				assert.Equal(t, "new", got.PayloadHash)
				return
			}
			exists, ok := errors.AsType[*idempotency.ItemAlreadyExistsError](err)
			require.True(t, ok, "%v", err)
			require.NotNil(t, exists.Old, "the record read is handed over")
			assert.Equal(t, old.Status, exists.Old.Status)
			assert.Equal(t, "abc", exists.Old.PayloadHash)
		})
	}
}

func TestOrphanLockedByAnotherInvocation(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	now := time.Now()
	require.NoError(t, server.Set("fn#h", "not a record"))
	require.NoError(t, server.Set("fn#h:lock", "True"))

	err := store.PutRecord(t.Context(), inProgress(now), now)
	exists, ok := errors.AsType[*idempotency.ItemAlreadyExistsError](err)
	require.True(t, ok, "%v", err)
	assert.Nil(t, exists.Old)
	got, err := server.Get("fn#h")
	require.NoError(t, err)
	assert.Equal(t, "not a record", got, "the lock holder replaces it")
}

func TestOrphanLockTimeout(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	now := time.Now()
	require.NoError(t, server.Set("fn#h", "{"))

	require.NoError(t, store.PutRecord(t.Context(), inProgress(now), now))
	assert.Equal(t, 10*time.Second, server.TTL("fn#h:lock"))
}

func TestCustomAttributeNames(t *testing.T) {
	t.Parallel()
	store, server := newStore(t,
		redis.WithExpiryAttr("ttl"),
		redis.WithInProgressExpiryAttr("lock_until"),
		redis.WithStatusAttr("state"),
		redis.WithDataAttr("result"),
		redis.WithValidationKeyAttr("hash"),
	)
	now := time.UnixMilli(1791633600500)

	require.NoError(t, store.PutRecord(t.Context(), inProgress(now), now))
	got, err := server.Get("fn#h")
	require.NoError(t, err)
	want := `{"state": "INPROGRESS", "ttl": 1791637200, "lock_until": 1791633630500, "hash": "abc"}`
	assert.Equal(t, want, got)

	record, err := store.GetRecord(t.Context(), "fn#h")
	require.NoError(t, err)
	assert.Equal(t, int64(1791637200), record.ExpiresAt.Unix(), "the custom expiry name is read back (Python reads \"expiration\")")
	assert.Equal(t, "abc", record.PayloadHash)
}

func TestMalformedValues(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"not JSON":          "{",
		"not an object":     "[1]",
		"no status":         `{"expiration": 1}`,
		"status not text":   `{"status": 1}`,
		"expiry not number": `{"status": "COMPLETED", "expiration": "soon"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, server := newStore(t)
			require.NoError(t, server.Set("fn#h", value))
			_, err := store.GetRecord(t.Context(), "fn#h")
			require.ErrorIs(t, err, idempotency.ErrMalformedRecord)
		})
	}
}

func TestDeleteRecord(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	now := time.Now()
	require.NoError(t, store.PutRecord(t.Context(), inProgress(now), now))
	require.NoError(t, store.DeleteRecord(t.Context(), "fn#h"))
	assert.False(t, server.Exists("fn#h"))
	require.NoError(t, store.DeleteRecord(t.Context(), "fn#h"), "deleting nothing is fine")
}

func TestServerErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	store, server := newStore(t)
	server.SetError("LOADING the dataset")
	now := time.Now()
	require.ErrorContains(t, store.PutRecord(t.Context(), inProgress(now), now), "LOADING")
	_, err := store.GetRecord(t.Context(), "fn#h")
	require.ErrorContains(t, err, "LOADING")
	require.ErrorContains(t, store.UpdateRecord(t.Context(), completed(now)), "LOADING")
	require.ErrorContains(t, store.DeleteRecord(t.Context(), "fn#h"), "LOADING")
}

func TestNewRejectsInvalidSettings(t *testing.T) {
	t.Parallel()
	_, err := redis.New(nil)
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)
	client := goredis.NewClient(&goredis.Options{Addr: "localhost:0"})
	_, err = redis.New(client, redis.WithDataAttr("status"))
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)
	_, err = redis.New(client, redis.WithValidationKeyAttr(""))
	require.ErrorIs(t, err, idempotency.ErrInvalidConfig)
}

func TestFromConnection(t *testing.T) {
	t.Parallel()
	server := miniredis.RunT(t)
	cases := map[string]redis.Connection{
		"url":       {URL: "redis://" + server.Addr()},
		"host":      {Host: "127.0.0.1", Port: mustPort(t, server), DisableTLS: true},
		"url wins":  {URL: "redis://" + server.Addr(), Host: "nowhere.invalid", Mode: redis.Standalone},
		"plain url": {URL: "redis://" + server.Addr() + "/0"},
	}
	for name, conn := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, err := redis.FromConnection(conn)
			require.NoError(t, err)
			_, err = store.GetRecord(t.Context(), "missing")
			require.ErrorIs(t, err, idempotency.ErrItemNotFound)
		})
	}
}

func mustPort(t *testing.T, server *miniredis.Miniredis) int {
	t.Helper()
	port, err := strconv.Atoi(server.Port())
	require.NoError(t, err)
	return port
}

func TestFromConnectionRejectsInvalidSettings(t *testing.T) {
	t.Parallel()
	for name, conn := range map[string]redis.Connection{
		"mode":        {Host: "localhost", Mode: "sentinel"},
		"url":         {URL: "http://localhost"},
		"cluster url": {URL: "http://localhost", Mode: redis.Cluster},
	} {
		_, err := redis.FromConnection(conn)
		require.ErrorIs(t, err, idempotency.ErrInvalidConfig, name)
	}
	for name, conn := range map[string]redis.Connection{
		"cluster":     {Host: "localhost", Mode: redis.Cluster},
		"cluster url": {URL: "rediss://localhost:7000?addr=localhost:7001", Mode: redis.Cluster},
		"tls":         {Host: "cache.example.com", Password: "secret"},
	} {
		store, err := redis.FromConnection(conn)
		require.NoError(t, err, name)
		assert.NotNil(t, store, name)
	}
}

// TestIdempotentOnRedis runs an idempotent function on the store, through
// payload validation, which Python's cache layer breaks on replays.
func TestIdempotentOnRedis(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	type payment struct {
		ID     string `json:"id"`
		Amount int    `json:"amount"`
	}
	runs := 0
	process := idempotency.Idempotent(func(_ context.Context, p payment) (string, error) {
		runs++
		return "paid " + p.ID, nil
	}, store, idempotency.WithEventKeyJMESPath("id"), idempotency.WithPayloadValidationJMESPath("amount"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	for range 2 {
		got, err := process(ctx, payment{ID: "p-1", Amount: 10})
		require.NoError(t, err)
		assert.Equal(t, "paid p-1", got)
	}
	assert.Equal(t, 1, runs)
	_, err := process(ctx, payment{ID: "p-1", Amount: 11})
	require.ErrorIs(t, err, idempotency.ErrPayloadValidation)
}
