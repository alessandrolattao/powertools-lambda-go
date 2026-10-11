package idempotency_test

import (
	"context"
	"fmt"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
)

type payment struct {
	UserID    string  `json:"user_id"`
	ProductID string  `json:"product_id"`
	Amount    float64 `json:"amount"`
}

type paymentResult struct {
	PaymentID string `json:"payment_id"`
	Message   string `json:"message"`
}

func ExampleIdempotent() {
	// In a Lambda function the store is DynamoDB or Redis; see the
	// idempotency/dynamodb and idempotency/redis packages.
	store := idempotency.NewMemoryStore()

	charges := 0
	handler := func(context.Context, payment) (paymentResult, error) {
		charges++
		return paymentResult{PaymentID: fmt.Sprintf("pay-%d", charges), Message: "success"}, nil
	}

	// Only the user and the product make a payment unique.
	charge := idempotency.Idempotent(handler, store,
		idempotency.WithEventKeyJMESPath("[user_id, product_id]"))
	// A Lambda function hands charge to lambda.Start.

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, p := range []payment{
		{UserID: "u-1", ProductID: "p-1", Amount: 10},
		{UserID: "u-1", ProductID: "p-1", Amount: 10}, // a retry: no second charge
		{UserID: "u-1", ProductID: "p-2", Amount: 10},
	} {
		result, err := charge(ctx, p)
		if err != nil {
			panic(err)
		}
		fmt.Println(p.ProductID, result.PaymentID)
	}
	fmt.Println("charges:", charges)
	// Output:
	// p-1 pay-1
	// p-1 pay-1
	// p-2 pay-2
	// charges: 2
}

func ExampleWithKeyPrefix() {
	type subscription struct {
		UserID int `json:"user_id"`
	}
	store := idempotency.NewMemoryStore()
	subscribe := idempotency.Idempotent(func(context.Context, subscription) (string, error) {
		return "subscribed", nil
	}, store, idempotency.WithKeyPrefix("my_custom_prefix"), idempotency.WithEventKeyJMESPath("user_id"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := subscribe(ctx, subscription{UserID: 1}); err != nil {
		panic(err)
	}
	// The key is the prefix and the MD5 of the selected JSON, 1 here: the
	// key a Python function with the same prefix computes.
	record, err := store.GetRecord(ctx, "my_custom_prefix#c4ca4238a0b923820dcc509a6f75849b")
	if err != nil {
		panic(err)
	}
	fmt.Println(record.Status, record.ResponseData)
	// Output: COMPLETED "subscribed"
}

func ExampleWithResponseHook() {
	process := idempotency.Idempotent(func(context.Context, payment) (map[string]string, error) {
		return map[string]string{"payment_id": "pay-1"}, nil
	}, idempotency.NewMemoryStore(), idempotency.WithResponseHook(
		func(_ context.Context, response map[string]string, _ idempotency.Record) (map[string]string, error) {
			response["x-idempotent-replay"] = "true"
			return response, nil
		}))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for range 2 {
		response, err := process(ctx, payment{UserID: "u-1"})
		if err != nil {
			panic(err)
		}
		fmt.Println(response)
	}
	// Output:
	// map[payment_id:pay-1]
	// map[payment_id:pay-1 x-idempotent-replay:true]
}
