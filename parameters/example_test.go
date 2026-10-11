package parameters_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
)

// vaultStore is a custom store, standing in for HashiCorp Vault, S3 or a
// database: Get and GetMultiple are all a provider needs.
type vaultStore struct {
	secrets map[string]string
}

func (v vaultStore) Get(_ context.Context, name string, _ parameters.StoreOptions) ([]byte, error) {
	value, ok := v.secrets[name]
	if !ok {
		return nil, parameters.ErrNotFound
	}
	return []byte(value), nil
}

func (v vaultStore) GetMultiple(_ context.Context, path string, _ parameters.StoreOptions) (map[string][]byte, error) {
	values := make(map[string][]byte)
	for name, value := range v.secrets {
		if rest, ok := strings.CutPrefix(name, path+"/"); ok {
			values[rest] = []byte(value)
		}
	}
	return values, nil
}

var vault = vaultStore{secrets: map[string]string{
	"/app/db":            `{"host":"db.internal","port":5432}`,
	"/app/api-key":       "k3y",
	"/app/feature.json":  `{"enabled":true}`,
	"/app/cert.binary":   "aGVsbG8=",
	"/app/region":        "eu-west-1",
	"/other/unrelated":   "x",
	"/app/nested/is/too": "deep",
}}

func ExampleNewProvider() {
	provider := parameters.NewProvider(vault)

	key, err := provider.Get(context.Background(), "/app/api-key", parameters.WithMaxAge(time.Minute))
	if err != nil {
		panic(err)
	}
	fmt.Println(key)

	_, err = provider.Get(context.Background(), "/app/missing")
	fmt.Println(errors.Is(err, parameters.ErrNotFound))
	// Output:
	// k3y
	// true
}

func ExampleGet() {
	type database struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	provider := parameters.NewProvider(vault)

	db, err := parameters.Get[database](context.Background(), provider, "/app/db", parameters.WithTransform(parameters.TransformJSON))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s:%d\n", db.Host, db.Port)
	// Output: db.internal:5432
}

func ExampleGetMultiple() {
	provider := parameters.NewProvider(vault)

	values, err := parameters.GetMultiple[any](context.Background(), provider, "/app", parameters.WithTransform(parameters.TransformAuto))
	if err != nil {
		panic(err)
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		fmt.Printf("%s: %v (%T)\n", name, values[name], values[name])
	}
	// Output:
	// api-key: k3y (string)
	// cert.binary: [104 101 108 108 111] ([]uint8)
	// db: {"host":"db.internal","port":5432} (string)
	// feature.json: map[enabled:true] (map[string]interface {})
	// nested/is/too: deep (string)
	// region: eu-west-1 (string)
}
