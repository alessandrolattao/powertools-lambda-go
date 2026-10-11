# Powertools for AWS Lambda (Go)

A Go port of [Powertools for AWS Lambda (Python)](https://docs.aws.amazon.com/powertools/python/latest/)
v3.35.0, the toolkit AWS maintains to implement serverless best practices.
Every Powertools utility is here, written the way Go is written today, and
compatible with the Python one on the wire: the same environment variables,
log keys, EMF documents and DynamoDB items.

```
go get github.com/alessandrolattao/powertools-lambda-go
```

It needs Go 1.27 (generic methods, `encoding/json/v2` on by default).

## Quick start

```go
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/tracing"
)

var (
	logger  = logging.New()
	tracer  = tracing.New()
	meter   = metrics.New(metrics.WithNamespace("Shop"))
)

func handle(ctx context.Context, e events.SQSEvent) (string, error) {
	logger.InfoContext(ctx, "batch received", "records", len(e.Records))
	tracing.PutAnnotation(ctx, "records", len(e.Records))
	if err := meter.AddMetric(ctx, "BatchReceived", metrics.Count, 1); err != nil {
		return "", err
	}
	return "ok", nil
}

func main() {
	lambda.Start(logger.InjectContext(tracer.CaptureLambdaHandler(meter.LogMetrics(handle))))
}
```

Every handler-level utility is a decorator: a function that takes a handler
and returns one, with the event and result types inferred. They stack with
each other and with your own middlewares.

## Utilities

| Package | What it does | Guide |
|---|---|---|
| `logging` | Structured JSON logs on `log/slog`, with Lambda context, cold start, correlation ids, sampling and buffering | [docs/logging.md](docs/logging.md) |
| `tracing` | OpenTelemetry traces exported to AWS X-Ray, annotations, metadata, AWS SDK and HTTP instrumentation | [docs/tracing.md](docs/tracing.md) |
| `metrics` | CloudWatch Embedded Metric Format; Datadog in `metrics/datadog` | [docs/metrics.md](docs/metrics.md) |
| `eventhandler` | Routing for API Gateway REST and HTTP, ALB, Function URLs, VPC Lattice, with validation and OpenAPI; AppSync, AppSync Events and Bedrock Agents resolvers | [docs/eventhandler.md](docs/eventhandler.md) |
| `batch` | Partial batch responses for SQS (standard and FIFO), Kinesis, DynamoDB Streams, Kafka | [docs/batch.md](docs/batch.md) |
| `idempotency` | Safe retries, with DynamoDB or Redis persistence | [docs/idempotency.md](docs/idempotency.md) |
| `circuitbreaker` | Stop calling an unhealthy dependency, with state shared in DynamoDB | [docs/circuitbreaker.md](docs/circuitbreaker.md) |
| `parameters` | SSM Parameter Store, Secrets Manager, AppConfig, DynamoDB, with caching and transforms | [docs/parameters.md](docs/parameters.md) |
| `featureflags` | Rule-based feature flags on AppConfig | [docs/featureflags.md](docs/featureflags.md) |
| `validation` | JSON Schema validation of events and responses | [docs/validation.md](docs/validation.md) |
| `parser` | Typed parsing of events, with envelopes | [docs/parser.md](docs/parser.md) |
| `eventsource` | The event types aws-lambda-go lacks, and helpers | [docs/eventsource.md](docs/eventsource.md) |
| `jmespath` | JMESPath with the Powertools decoding functions and envelopes | [docs/jmespath.md](docs/jmespath.md) |
| `streaming` | S3 objects larger than memory, as seekable streams with transforms | [docs/streaming.md](docs/streaming.md) |
| `kafka` | Kafka events from MSK and self-managed clusters, with Avro, Protobuf and JSON | [docs/kafka.md](docs/kafka.md) |
| `datamasking` | Erase or encrypt sensitive fields, with the AWS Encryption SDK in `datamasking/kms` | [docs/datamasking.md](docs/datamasking.md) |
| `auth`, `oauth2` | JWT access-token verification and authorizers; client-credentials tokens | [docs/auth.md](docs/auth.md), [docs/oauth2.md](docs/oauth2.md) |
| `metadata` | The Lambda Metadata Endpoint | [docs/metadata.md](docs/metadata.md) |
| `middleware` | Your own handler middlewares | [docs/middleware.md](docs/middleware.md) |

Lambda Managed Instances and durable functions:
[docs/lambda-features.md](docs/lambda-features.md).

## Design

The principles, the conventions every package follows, and the decisions
behind the differences from Python are in [docs/design.md](docs/design.md).
In short:

- **Parity, idiomatically.** Every Powertools feature, with Python's
  defaults and `POWERTOOLS_*` variables; nothing that Go, aws-lambda-go or
  aws-sdk-go-v2 already provide (Python's typing and event data classes have
  no reason to exist in Go).
- **Safe under concurrency.** Lambda Managed Instances run several
  invocations in one process, so per-invocation state lives in the
  `context.Context`.
- **Upstream bugs are fixed, not copied**; each package's documentation lists
  where it differs from Python, and why.
- **OpenTelemetry**, not the X-Ray SDK, as the AWS migration guide
  prescribes.

## Development

```
go test -race ./...                 # unit tests, examples
cd integration && go test -race ./... # against Floci (AWS emulator) and Valkey, needs Docker
golangci-lint run ./...             # strict configuration, see .golangci.yml
```

Fuzz tests cover every parser of untrusted input:
`go test -run=^$ -fuzz=FuzzSearch ./jmespath/`.

## License

MIT, see [LICENSE](LICENSE). Powertools for AWS Lambda (Python), which this
port follows, is MIT-0. The Swagger UI assets embedded in
`eventhandler/swaggerui` are Apache-2.0, with their own LICENSE and NOTICE.
