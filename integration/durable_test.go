package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-durable-execution-sdk-go/durable"
	"github.com/aws/aws-durable-execution-sdk-go/durable/durabletest"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/tracing"
)

// durableEntry registers a raw payload handler with aws-lambda-go, as
// durable.Start does: the durable invocation payload must arrive byte-exact.
type durableEntry func(ctx context.Context, payload []byte) ([]byte, error)

func (f durableEntry) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	return f(ctx, payload)
}

// chargeWorkflow is a durable function that charges an order in a step, then
// waits, which ends the first invocation and replays the handler in a second
// one. The step's work is captured inside the step body, where it runs once.
func chargeWorkflow(tracer *tracing.Tracer) durable.Handler[string, string] {
	return func(ctx durable.Context, order string) (string, error) {
		charged, err := durable.Step(ctx, "charge", func(step durable.StepContext) (string, error) {
			return tracer.Capture(step, "charge", func(spanCtx context.Context) (string, error) {
				// Each invocation of an execution is a trace of its own: the
				// execution ARN ties them together.
				tracing.PutAnnotation(spanCtx, "execution_arn", ctx.ExecutionArn())
				return "charged " + order, nil
			})
		})
		if err != nil {
			return "", err
		}
		if err := durable.Wait(ctx, "cooldown", time.Second); err != nil {
			return "", err
		}
		return charged, nil
	}
}

func TestTracingDurableExecution(t *testing.T) {
	t.Parallel()
	tracer, recorder := recordingTracer(t)
	workflow := chargeWorkflow(tracer)

	// In main, the invocation is traced outside the durable execution:
	//
	//	lambda.StartHandler(durableEntry(tracer.CaptureLambdaHandler(durable.Wrap(workflow))))
	var _ lambda.Handler = durableEntry(tracer.CaptureLambdaHandler(durable.Wrap(workflow)))

	result, err := durabletest.NewLocalRunner(workflow).RunUntilComplete("o-1")
	require.NoError(t, err)
	out, err := durabletest.ResultAs[string](result)
	require.NoError(t, err)
	assert.Equal(t, "charged o-1", out)
	assert.Greater(t, len(result.Invocations), 1, "the wait split the execution into invocations")

	var charges int
	for _, span := range recorder.Ended() {
		if span.Name() != "## charge" {
			continue
		}
		charges++
		for _, kv := range span.Attributes() {
			if kv.Key == "execution_arn" {
				assert.NotEmpty(t, kv.Value.AsString())
			}
		}
	}
	assert.Equal(t, 1, charges, "a replayed step does not run, nor trace, again")
}
