// Package integration tests the utilities that talk to AWS, and to Redis,
// against real implementations of those protocols: Floci, an AWS emulator,
// and Valkey. It is a module of its own, so that the testcontainers
// dependencies stay out of the library's.
//
// Run it with `go test ./...` from this directory; it needs Docker.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	floci "github.com/floci-io/testcontainers-floci-go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/valkey"
)

// The images the suite runs, pinned so that a run is reproducible.
const (
	flociImage  = "floci/floci:2.1.0"
	valkeyImage = "valkey/valkey:8"
)

// suite is what every test shares: the containers start once per run.
var suite struct {
	aws       aws.Config
	valkeyURL string
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	emulator, err := floci.Run(ctx, flociImage)
	defer terminate(emulator)
	if err != nil {
		return fail("start Floci", err)
	}
	suite.aws, err = config.LoadDefaultConfig(ctx,
		config.WithRegion(emulator.GetRegion()),
		config.WithBaseEndpoint(emulator.GetEndpoint()),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			emulator.GetAccessKey(), emulator.GetSecretKey(), "")),
	)
	if err != nil {
		return fail("load the AWS config", err)
	}

	cache, err := valkey.Run(ctx, valkeyImage)
	defer terminate(cache)
	if err != nil {
		return fail("start Valkey", err)
	}
	suite.valkeyURL, err = cache.ConnectionString(ctx)
	if err != nil {
		return fail("read the Valkey address", err)
	}

	return m.Run()
}

// terminate stops c; testcontainers ignores a container that never started.
func terminate(c testcontainers.Container) {
	if err := testcontainers.TerminateContainer(c); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "terminate container:", err)
	}
}

func fail(step string, err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "integration: %s: %v\n", step, err)
	return 1
}
