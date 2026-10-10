# Design

Powertools for AWS Lambda (Go) is a port of Powertools for AWS Lambda
(Python) v3.35.0, the most complete of the official runtimes. It brings every
Powertools capability to Go, written the way Go is written today.

## Principles

1. **Parity, idiomatically.** Every Powertools feature has a Go equivalent with
   the same behavior, the same defaults and the same environment variables.
   Nothing is ported that Go, aws-lambda-go or aws-sdk-go-v2 already provide
   (see [Not ported](#not-ported)).
2. **Wire compatibility.** Whatever leaves the function looks the way
   Powertools Python writes it: log record keys, EMF documents, idempotency
   and circuit breaker items in DynamoDB, feature flag schemas, batch
   responses. A Go function and a Python function can share a log query, a
   dashboard, a table.
3. **Explicit option > environment variable > default.** The `POWERTOOLS_*`
   variables keep their Python names and accepted values.
4. **Safe under concurrency.** Lambda Managed Instances run several
   invocations in one process. Per-invocation state lives in the
   `context.Context`, never in package variables.
5. **The standard library first.** `log/slog`, `encoding/json/v2`, `iter`,
   `uuid`, `net/http`. OpenTelemetry for traces, exported to X-Ray as the AWS
   X-Ray SDK migration guide prescribes.

## Layout

One package per utility, named after its Python module:

| Package | Python | |
|---|---|---|
| `logging` | `aws_lambda_powertools.logging` | structured logger on `log/slog` |
| `tracing` | `aws_lambda_powertools.tracing` | tracer on OpenTelemetry, exported to X-Ray |
| `metrics` | `aws_lambda_powertools.metrics` | CloudWatch EMF metrics; `metrics/datadog` |
| `middleware` | `middleware_factory` | handler middlewares |
| `eventhandler` | `event_handler` | REST, HTTP, ALB, Function URL, VPC Lattice resolvers; `appsync`, `appsyncevents`, `bedrockagent`, `openapi` below it |
| `batch` | `utilities.batch` | SQS, Kinesis, DynamoDB Streams partial failures |
| `idempotency` | `utilities.idempotency` | `idempotency/dynamodb`, `idempotency/redis` persistence |
| `circuitbreaker` | `utilities.circuit_breaker` | |
| `parameters` | `utilities.parameters` | `parameters/ssm`, `parameters/secrets`, `parameters/appconfig`, `parameters/dynamodb` |
| `featureflags` | `utilities.feature_flags` | |
| `validation` | `utilities.validation` | JSON Schema |
| `parser` | `utilities.parser` | typed parsing and envelopes |
| `eventsource` | `utilities.data_classes` | only the event types aws-lambda-go lacks, and helpers |
| `jmespath` | `utilities.jmespath_utils` | JMESPath with the Powertools functions |
| `streaming` | `utilities.streaming` | |
| `kafka` | `utilities.kafka` | |
| `datamasking` | `utilities.data_masking` | |
| `auth`, `oauth2` | `utilities.auth_alpha`, OAuth2 client | |
| `metadata` | `utilities.metadata` | |

`internal/env` reads the environment, `internal/coldstart` decides the cold
invocation, `internal/debug` is the `POWERTOOLS_DEBUG` logger.

## Conventions

### Handlers and decorators

A handler is `func(context.Context, E) (R, error)`, the shape `lambda.Start`
accepts (`middleware.Handler[E, R]`). A Python decorator becomes a function,
or a generic method, that takes a handler and returns one:

```go
h := logger.InjectContext(metrics.LogMetrics(tracer.CaptureLambdaHandler(handle)))
lambda.Start(h)
```

Type parameters are inferred from the handler. Options follow the handler as
variadic functional options.

### Configuration

Constructors take functional options (`New(opts ...Option)`). Every option
that has a `POWERTOOLS_*` variable falls back to it, then to the Python
default. Invalid variable values warn and fall back; they never stop a
function from starting.

### AWS clients

A utility that calls AWS depends on a narrow interface holding only the
methods it calls, satisfied by the aws-sdk-go-v2 client (`*ssm.Client`). The
constructor takes the client; a `FromConfig(aws.Config)` convenience builds
it. Package-level shortcuts (`parameters.GetParameter`) use a default
provider built lazily from `config.LoadDefaultConfig`, as Python does.

### Errors

Sentinel errors are `ErrXxx` variables, error types are `XxxError` structs
with `Unwrap` where they wrap. Callers use `errors.Is` and `errors.AsType`.
Errors from other packages are wrapped with context (`fmt.Errorf("...: %w")`).

### JSON

`encoding/json/v2` and `encoding/json/jsontext` only. Output that is compared
or hashed (idempotency payloads) is marshalled with
`json.Deterministic(true)`.

### Logging inside Powertools

Utilities never log to the user's logger. Internal diagnostics go to
`internal/debug`, silent unless `POWERTOOLS_DEBUG` is on.

### Tests and documentation

- Every package: unit tests (parallel where they can be, `-race` clean),
  testable examples (`Example...` with `// Output:`), a `doc.go` with the
  package overview, fuzz tests for every parser of untrusted input.
- AWS-backed behavior is tested against Floci (`floci/floci`, an AWS
  emulator) and Valkey through testcontainers-go, in the `integration`
  module.
- `golangci-lint run ./...` reports 0 issues under `.golangci.yml`. A
  `//nolint` names the linter and says why.

## Decisions

Answers to the questions the port raised, so that every package answers
them the same way.

- **Upstream bugs are fixed, not copied**, and the package documentation
  says what differs. Examples: the Redis idempotency layer forgetting the
  validation hash, Kafka Protobuf message indexes read without zigzag
  decoding.
- **Decoding is lenient**, like aws-lambda-go: unknown fields are ignored,
  missing ones stay zero. Validation is opt-in: `parser` calls
  `Validate() error` on a target that has it, and `validation` checks JSON
  Schema.
- **JSON v2 matches names case-sensitively.** Where AWS spells one field two
  ways (SNS `UnsubscribeUrl` / `UnsubscribeURL` through SQS), decoding uses
  `json.MatchCaseInsensitiveNames(true)`.
- **No network unless asked.** JSON Schema `$ref` to remote URLs is not
  fetched by default; a loader option enables it.
- **Panics are bugs, but a batch survives them.** Batch processing recovers a
  panicking record handler and reports the record as failed. Idempotency
  deletes the in-progress record and re-panics. Middlewares never swallow
  panics otherwise.
- **Cancellation.** When the invocation's context ends, batch processing stops
  and reports the unprocessed records as failed; idempotency treats it as an
  error of the wrapped function.
- **Idempotency hashes JSON the way Python does** (`json.dumps` with sorted
  keys, its separators and `ensure_ascii`), with MD5 by default, so that a Go
  and a Python function can share records when they share a key prefix. The
  typed input is marshalled with `encoding/json/v2` first: fields the
  function's type drops do not take part in the key.
- **Redis means go-redis v9** (`redis.UniversalClient`: standalone, cluster,
  sentinel; Valkey speaks the same protocol), as Python means redis-py.
- **Circuit breakers are values** created once (in `main` or at package
  level). Each owns its failure counter; the half-open probe owner is the
  breaker instance.
- **Values that change shape** (a parameter that is a string, bytes or a
  JSON document) are read through generic functions: the caller names the Go
  type, the transform decodes into it.
- **Ordered configuration is a slice**, never a map (masking rules, routes).
- **Time zones are embedded** (`time/tzdata`) by the packages that evaluate
  time conditions, since Lambda's OS images do not guarantee a zone database.
  Such packages take an injectable clock for tests.
- **Libraries.** JMESPath: `github.com/jmespath-community/go-jmespath`.
  JSON Schema: `github.com/santhosh-tekuri/jsonschema/v6`. Avro:
  `github.com/twmb/avro` (`hamba/avro` is archived). Protobuf:
  `google.golang.org/protobuf`. JWT: `github.com/lestrrat-go/jwx/v4`, with
  our own JWKS cache (Powertools never serves keys whose refresh failed).
  OAuth2: `golang.org/x/oauth2/clientcredentials` for the token request, our
  own caching. Encryption: the AWS Encryption SDK for Go, interoperable with
  the Python SDK. JSONPath: `github.com/ohler55/ojg`.

## Not ported

| Python | Why |
|---|---|
| Typing (`LambdaContext` type hints) | Go is statically typed; `lambdacontext.LambdaContext` is the context. |
| Event source data classes | `github.com/aws/aws-lambda-go/events` has the event structs; `eventsource` adds only the missing ones. |
| Pydantic models | Go structs with `encoding/json/v2` are the models; `parser` adds envelopes and validation. |
| X-Ray SDK patching (`patch_modules`) | OpenTelemetry instrumentation of the AWS SDK and `net/http` replaces it. |
| Boto3 user agent feature tracking | It reports usage of the official Powertools; an unofficial port must not pose as one. |
