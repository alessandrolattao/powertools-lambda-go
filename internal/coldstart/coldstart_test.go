package coldstart

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

func invocation(requestID string) context.Context {
	return lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{AwsRequestID: requestID})
}

// The tests share the process-wide claim, so none of them runs in parallel.

func TestIsClaimsTheFirstInvocation(t *testing.T) {
	t.Setenv(env.LambdaInitializationType, "on-demand")
	Reset()
	t.Cleanup(Reset)

	assert.True(t, Is(invocation("first")))
	assert.True(t, Is(invocation("first")), "every utility asking during the cold invocation agrees")
	assert.False(t, Is(invocation("second")))
	assert.True(t, Is(invocation("first")))
}

func TestIsWithoutARequestID(t *testing.T) {
	t.Setenv(env.LambdaInitializationType, "on-demand")
	Reset()
	t.Cleanup(Reset)

	assert.True(t, Is(context.Background()))
	assert.False(t, Is(context.Background()), "only one invocation without a request id can be cold")
	assert.False(t, Is(invocation("later")))
}

func TestIsUnderProvisionedConcurrency(t *testing.T) {
	t.Setenv(env.LambdaInitializationType, "provisioned-concurrency")
	Reset()
	t.Cleanup(Reset)

	assert.False(t, Is(invocation("first")))
}

func TestIsConcurrentInvocations(t *testing.T) {
	t.Setenv(env.LambdaInitializationType, "on-demand")
	Reset()
	t.Cleanup(Reset)

	var cold atomic.Int32
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			if Is(invocation("request-" + strconv.Itoa(i))) {
				cold.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), cold.Load(), "exactly one concurrent invocation is cold")
}
