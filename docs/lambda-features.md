# Lambda features

How Powertools for AWS Lambda (Go) behaves with two Lambda features that
change the execution model: Lambda Managed Instances and durable functions.

## Lambda Managed Instances

[Lambda Managed Instances](https://docs.aws.amazon.com/lambda/latest/dg/lambda-managed-instances.html)
run functions on EC2 instances, with several invocations at once in one
execution environment.

### Key differences from Lambda On Demand

| Aspect | Lambda On Demand | Lambda Managed Instances |
|---|---|---|
| **Concurrency** | One invocation per execution environment | Several concurrent invocations per environment |
| **Python model** | One process, one request | Several processes, one request each |
| **Go model** | One process, one request | One process, one goroutine per concurrent request |
| **Initialization type** | `on-demand`, `provisioned-concurrency` | `lambda-managed-instances` |

Python isolates concurrent requests in separate processes, so its module-level
state stays per request and Powertools Python needs no change. Go is the
opposite: aws-lambda-go runs `AWS_LAMBDA_MAX_CONCURRENCY` invoke loops as
goroutines of one process, sharing every package variable.

### What Powertools Go does about it

* **Per-invocation state lives in the `context.Context`**, never in package
  variables: concurrent invocations cannot see each other's state.
* **The trace header comes from the context.** aws-lambda-go sets
  `_X_AMZN_TRACE_ID` only when the environment runs one invocation at a time,
  but always puts the header in the invocation's context. The tracer parents
  each invocation span on that header, so concurrent invocations join their
  own traces; the environment variable is only a last resort.
* **One cold start.** Concurrent first invocations race for it; exactly one
  wins, and every utility agrees on which.
* **One writer for standard output.** Log lines and metric documents of
  concurrent invocations are written whole, never interleaved.
* **Shared caches are locked.** Caches are per process, shared by the
  concurrent invocations of the environment. Concurrent first calls of the
  metadata utility share one request to the Lambda Metadata Endpoint.
* **Annotations from several goroutines.** `tracing.PutAnnotation` on one span
  from concurrent goroutines keeps every key.

### Tracer

Nothing changes in your code:

```go
var tracer = tracing.New(tracing.WithService("payment"))

func main() {
	lambda.Start(tracer.CaptureLambdaHandler(handle))
}
```

### VPC connectivity

Managed Instances run in your VPC. Sending logs to CloudWatch Logs, traces to
X-Ray and calling AWS services (SSM, Secrets Manager, DynamoDB) needs network
connectivity, through one of:

1. **VPC endpoints**: private connectivity without internet access
2. **NAT gateway**: internet access from private subnets
3. **Public subnet with an internet gateway**
4. **Egress-only internet gateway**: IPv6 outbound connectivity

See
[Networking for Lambda Managed Instances](https://docs.aws.amazon.com/lambda/latest/dg/lambda-managed-instances-networking.html).

### FAQ

**Is my code safe for concurrent use?** Powertools is. Your own package
variables are shared by concurrent invocations: guard them, or keep
per-invocation state in the context.

**Why is a cache warmed once per environment, not per request?** Go caches are
per process, and one process serves every concurrent invocation of the
environment: fewer calls to backends than Python, which warms one cache per
process.

## Durable functions

[Durable functions](https://docs.aws.amazon.com/lambda/latest/dg/durable-functions.html)
checkpoint their progress and replay the handler from the start in each
invocation, skipping the operations already completed. In Go they are written
with the [AWS Durable Execution SDK for Go](https://github.com/aws/aws-durable-execution-sdk-go),
an experimental preview: Powertools does not depend on it, and works with it
through the standard interfaces.

A `durable.Context` is a `context.Context` that wraps the invocation's
context, so `lambdacontext.FromContext`, the tracer's spans and every
Powertools utility find what they need in it. Python's durable context
unwrapping is not needed.

### Tracer

Trace the invocation outside the durable execution, as Python stacks
`@tracer.capture_lambda_handler` above `@durable_execution`. `durable.Wrap`
returns the raw payload handler `durable.Start` registers;
`CaptureLambdaHandler` decorates it, and a one-method type registers it with
aws-lambda-go:

```go
// durableEntry registers a raw payload handler with aws-lambda-go, as
// durable.Start does: the durable invocation payload must arrive byte-exact.
type durableEntry func(ctx context.Context, payload []byte) ([]byte, error)

func (f durableEntry) Invoke(ctx context.Context, payload []byte) ([]byte, error) {
	return f(ctx, payload)
}

func main() {
	lambda.StartHandler(durableEntry(tracer.CaptureLambdaHandler(durable.Wrap(workflow))))
}
```

Capture work **inside step bodies**, where it runs once:

```go
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
```

???+ note "Trace continuity"
    Each invocation of an execution, replays included, is a trace of its own.
    Annotate the execution ARN (`ctx.ExecutionArn()`) to find them all with a
    filter expression.

???+ warning "Do not capture durable operations"
    When an execution suspends (on a wait, a callback, a pending step), the
    SDK unwinds the handler by returning an internal error from the blocked
    operation. A `Capture` around `durable.Step` or `durable.Wait` would
    record that error as a failure. Capture the work inside step bodies, and
    leave the invocation to `CaptureLambdaHandler`, which sees the SDK's
    PENDING response, not an error.

### Logger

The SDK's `Context.Logger` deduplicates log records across replays and adds
the execution ARN, the operation and the attempt. It accepts any
`slog.Handler` through `durable.WithLogHandler`: give it the Powertools
logger's handler to keep the Powertools record format. Records written with a
logger outside the durable context repeat on every replay.

### Metrics

Metrics emitted in the handler body are emitted again on every replay. Emit
them inside a step, or when the workflow completes, to count executions
rather than invocations.

### Metadata

The Availability Zone of the execution environment can differ between the
invocations of one execution: read it with `metadata.Get` where it is used,
not inside a step whose result is checkpointed and replayed.

### FAQ

**Do I need idempotency with durable functions?** Steps are already
idempotent through checkpoints. Idempotency still protects the handler entry
point from duplicate events, such as those of event source mappings.

**Why do I see one trace per invocation?** Each invocation, replays included,
gets its own trace from Lambda. Correlate them with the execution ARN.
