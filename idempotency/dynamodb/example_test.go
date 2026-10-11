package dynamodb_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/dynamodb"
)

func ExampleFromConfig() {
	// In a Lambda function: cfg, err := config.LoadDefaultConfig(ctx).
	cfg := aws.Config{Region: "eu-west-1"}

	store, err := dynamodb.FromConfig(cfg, "IdempotencyTable")
	if err != nil {
		panic(err)
	}
	handler := idempotency.Idempotent(func(_ context.Context, orderID string) (string, error) {
		return "processed " + orderID, nil
	}, store)
	fmt.Printf("%T\n", handler)
	// Output: func(context.Context, string) (string, error)
}

func ExampleWithSortKeyAttr() {
	// A table whose primary key is "id" and "sort_key"; a fake one here.
	table := newFakeDynamoDB("id", "sort_key")
	store, err := dynamodb.New(table, "IdempotencyTable",
		dynamodb.WithSortKeyAttr("sort_key"),
		dynamodb.WithStaticPKValue("idempotency#MyLambdaFunction"))
	if err != nil {
		panic(err)
	}
	process := idempotency.Idempotent(func(_ context.Context, userID int) (map[string]any, error) {
		return map[string]any{"user_id": userID, "message": "success"}, nil
	}, store, idempotency.WithKeyPrefix("orders"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := process(ctx, 12391); err != nil {
		panic(err)
	}
	item := table.onlyItem()
	for _, name := range []string{"id", "sort_key", "status", "data"} {
		if value, ok := item[name].(*types.AttributeValueMemberS); ok {
			fmt.Printf("%s: %s\n", name, value.Value)
		}
	}
	fmt.Println(slices.Sorted(maps.Keys(item)))
	// Output:
	// id: idempotency#MyLambdaFunction
	// sort_key: orders#47c00b40b245c5389dfc3100ebb4b7a0
	// status: COMPLETED
	// data: {"message":"success","user_id":12391}
	// [data expiration id in_progress_expiration sort_key status]
}
