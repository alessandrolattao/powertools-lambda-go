package secrets_test

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/alessandrolattao/powertools-lambda-go/parameters/secrets"
)

func ExampleProvider_GetByName() {
	// In a function: secrets.FromConfig(cfg), created once in main.
	provider := secrets.New(newFake())

	values, failed, err := provider.GetByName(context.Background(), []string{"api-key", "db-password", "missing"})
	if err != nil {
		panic(err)
	}
	fmt.Println(values["api-key"], values["db-password"])
	fmt.Println("failed:", slices.Sorted(maps.Keys(failed)))
	// Output:
	// k3y s3cret
	// failed: [missing]
}
