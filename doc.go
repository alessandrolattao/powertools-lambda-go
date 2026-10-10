// Package powertools is Powertools for AWS Lambda (Go): a port of Powertools
// for AWS Lambda (Python), the toolkit AWS maintains to implement serverless
// best practices, written the way Go is written today.
//
// Every utility lives in its own package:
//
//   - logging: structured JSON logs with Lambda context, sampling, buffering
//   - tracing: OpenTelemetry traces exported to AWS X-Ray
//   - metrics: CloudWatch Embedded Metric Format, and Datadog
//   - eventhandler: routing for API Gateway, ALB, Function URLs, VPC Lattice,
//     AppSync, Bedrock Agents, with validation and OpenAPI
//   - batch: partial batch responses for SQS, Kinesis, DynamoDB Streams, Kafka
//   - idempotency: safe retries, on DynamoDB or Redis
//   - circuitbreaker: stop calling an unhealthy dependency
//   - parameters: SSM Parameter Store, Secrets Manager, AppConfig, DynamoDB
//   - featureflags: rule-based feature flags on AppConfig
//   - validation: JSON Schema validation of events and responses
//   - parser: typed parsing of events, with envelopes
//   - eventsource: the event types aws-lambda-go lacks
//   - jmespath: JMESPath with the Powertools decoding functions
//   - streaming: S3 objects larger than memory, as seekable streams
//   - kafka: Kafka events with Avro, Protobuf and JSON
//   - datamasking: erase or encrypt sensitive fields
//   - auth, oauth2: JWT verification and client-credentials tokens
//   - metadata: the Lambda Metadata Endpoint
//   - middleware: your own handler middlewares
//
// The utilities read the same POWERTOOLS_* environment variables as the
// Python ones, write the same log keys, EMF documents and DynamoDB items, and
// are safe for the concurrent invocations of Lambda Managed Instances. They
// need Go 1.27, where encoding/json/v2 is enabled by default.
package powertools

import (
	"context"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// Version is the version of this module.
const Version = "0.1.0"

// ColdStart reports whether the invocation ctx belongs to is the one that
// paid for the execution environment's initialization. It is exact when
// called while a Powertools handler decorator is running; see
// internal/coldstart for how the cold invocation is claimed.
func ColdStart(ctx context.Context) bool {
	return coldstart.Is(ctx)
}

// ServiceName returns the service the function belongs to:
// POWERTOOLS_SERVICE_NAME, or "service_undefined" as Powertools Python names
// a function without one.
func ServiceName() string {
	return env.ServiceNameOr(env.DefaultServiceName)
}
