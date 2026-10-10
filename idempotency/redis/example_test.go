package redis_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/redis"
)

func ExampleNew() {
	// An in-process Redis stands for ElastiCache or MemoryDB here.
	server, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer server.Close()

	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }() //nolint:errcheck // closing at exit
	store, err := redis.New(client)
	if err != nil {
		panic(err)
	}

	process := idempotency.Idempotent(func(_ context.Context, paymentID int) (map[string]any, error) {
		return map[string]any{"payment_id": paymentID, "message": "success"}, nil
	}, store, idempotency.WithKeyPrefix("payments"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err = process(ctx, 12345); err != nil {
		panic(err)
	}
	value, err := server.Get("payments#827ccb0eea8a706c4c34a16891f84e7b")
	if err != nil {
		panic(err)
	}
	// The value is the JSON Python writes; the expiry changes with the clock.
	fields, _, _ := strings.Cut(value, `, "expiration"`)
	fmt.Println(fields + ", ...}")
	// Output: {"data": "{\"message\":\"success\",\"payment_id\":12345}", "status": "COMPLETED", ...}
}

func ExampleFromConnection() {
	store, err := redis.FromConnection(redis.Connection{
		Host:     "my-cache.serverless.euw1.cache.amazonaws.com",
		Username: "lambda",
		Password: "from Secrets Manager",
		Mode:     redis.Cluster,
	})
	fmt.Println(store != nil, err)
	// Output: true <nil>
}
