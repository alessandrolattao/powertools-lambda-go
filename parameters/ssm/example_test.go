package ssm_test

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

func ExampleProvider_GetByName() {
	// In a function: ssm.FromConfig(cfg), created once in main.
	provider := ssm.New(newFakeSSM())

	values, failed, err := provider.GetByName(context.Background(), map[string][]parameters.Option{
		"/app/db/host":     nil,
		"/app/db/password": {ssm.WithDecrypt(true)},
		"/app/missing":     nil,
	}, ssm.WithRaiseOnError(false))
	if err != nil {
		panic(err)
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		fmt.Println(name, "=", values[name])
	}
	fmt.Println("failed:", slices.Sorted(maps.Keys(failed)))
	// Output:
	// /app/db/host = db.local
	// /app/db/password = s3cret
	// failed: [/app/missing]
}

func ExampleProvider_GetMultiple() {
	provider := ssm.New(newFakeSSM())

	values, err := provider.GetMultiple(context.Background(), "/app/db", ssm.WithDecrypt(true))
	if err != nil {
		panic(err)
	}
	fmt.Println(values["host"], values["port"], values["password"])
	// Output: db.local 5432 s3cret
}
