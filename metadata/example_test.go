package metadata_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/metadata"
)

func ExampleGet() {
	// In Lambda, Get asks the Lambda Metadata Endpoint once and caches the
	// answer; here a local server plays the endpoint.
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"AvailabilityZoneID":"use1-az1"}`)) //nolint:errcheck // a test server
	}))
	defer endpoint.Close()
	lambdaEnv := map[string]string{
		"AWS_LAMBDA_INITIALIZATION_TYPE": "on-demand",
		"AWS_LAMBDA_METADATA_API":        strings.TrimPrefix(endpoint.URL, "http://"),
		"AWS_LAMBDA_METADATA_TOKEN":      "token",
	}
	for name, value := range lambdaEnv {
		_ = os.Setenv(name, value) //nolint:errcheck // valid names
	}
	defer func() {
		for name := range lambdaEnv {
			_ = os.Unsetenv(name) //nolint:errcheck // valid names
		}
		metadata.ClearCache()
	}()

	md, err := metadata.Get(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(md.AvailabilityZoneID)
	// Output: use1-az1
}

func ExampleGet_outsideLambda() {
	// Outside Lambda there is no endpoint: Get returns empty metadata, so
	// code that uses it runs in tests and on a laptop unchanged.
	md, err := metadata.Get(context.Background())
	fmt.Printf("%q %v\n", md.AvailabilityZoneID, err)
	// Output: "" <nil>
}
