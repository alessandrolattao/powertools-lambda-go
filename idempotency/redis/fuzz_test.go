package redis_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/redis"
)

// FuzzRecordValues stores arbitrary values under a key, as a cache shared
// with other writers may hold: reading them and writing over them returns a
// record or an error, never panics.
func FuzzRecordValues(f *testing.F) {
	f.Add(`{"status": "COMPLETED", "expiration": 1636549553, "data": "{}"}`)
	f.Add(`{"status": "INPROGRESS", "expiration": 1e30, "in_progress_expiration": -1}`)
	f.Add(`{"status": null, "validation": 1}`)
	f.Add(`not json`)
	server := miniredis.NewMiniRedis()
	if err := server.Start(); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(server.Close)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	store, err := redis.New(client)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, value string) {
		server.FlushAll()
		if err := server.Set("k", value); err != nil {
			t.Fatal(err)
		}
		_, _ = store.GetRecord(t.Context(), "k") //nolint:errcheck // the property is the absence of panics
		now := time.Now()
		record := idempotency.Record{Key: "k", Status: idempotency.StatusInProgress, ExpiresAt: now.Add(time.Hour)}
		_ = store.PutRecord(t.Context(), record, now) //nolint:errcheck // the property is the absence of panics
	})
}
