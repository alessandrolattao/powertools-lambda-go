package dynamodb_test

import (
	"context"
	"fmt"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/dynamodb"
)

func ExampleNew() {
	// In a function: dynamodb.FromConfig(cfg, "ParameterTable"), created
	// once in main.
	provider := dynamodb.New(newTable(), "ParameterTable")

	value, err := provider.Get(context.Background(), "my-param")
	if err != nil {
		panic(err)
	}
	all, err := parameters.GetMultiple[string](context.Background(), provider, "my-hash-key")
	if err != nil {
		panic(err)
	}
	fmt.Println(value, all["param-a"], all["param-b"])
	// Output: my-value my-value-a my-value-b
}
