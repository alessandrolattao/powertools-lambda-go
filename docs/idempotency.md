# Idempotency

The idempotency utility makes a Lambda handler, or any function, safe to retry: called again with the same input
within a time window, it returns the result of the first call instead of running again.

It is the Go side of the idempotency utility of Powertools for AWS Lambda (Python), and writes the same records, so
that a Go and a Python function can share a table or a cache.

## Key features

* Returns the previous successful result when a function is called again with the same idempotency key
* Takes the idempotency key from one or more fields of the payload, or from the whole payload
* Guards against concurrent requests, timeouts, missing idempotency keys and payload tampering
* Stores records in Amazon DynamoDB, Valkey, Redis OSS or any Redis-compatible cache, or in a store of your own
* Computes the same keys as Powertools Python, and reads and writes the same records

## Terminology

**Idempotency key**: by default, the Lambda function's name, the Go function's qualified name and a hash of the
payload (or of the parts of it you choose): `my-function.github.com/me/app/payments.charge#c4ca4238a0b923820dcc509a6f75849b`.
A [custom prefix](#customizing-the-idempotency-key-generation) replaces the names.

**Idempotent request**: a request whose key has a record that has not expired yet.

**Persistence layer**: the `Store` that creates, reads, completes and deletes records.

**Idempotency record**: what the store keeps for a key, an `idempotency.Record`:

| Field | Content |
|---|---|
| `Key` | the idempotency key |
| `Status` | `INPROGRESS` while the function runs, `COMPLETED` after it succeeded (`EXPIRED` is derived, never stored) |
| `ExpiresAt` | when the record expires, in whole seconds |
| `InProgressExpiresAt` | when an in-progress record stops locking the key: the invocation's deadline |
| `ResponseData` | the function's result, as JSON |
| `PayloadHash` | the hash of the validated part of the payload, with [payload validation](#payload-validation) |

## Getting started

We use Amazon DynamoDB in the examples. For a cache, see [Redis and Valkey](#redis-and-valkey).

### IAM permissions

| IAM permission | Operation |
|---|---|
| `dynamodb:GetItem` | read a record (strongly consistent) |
| `dynamodb:PutItem` | create a record, replace an expired one |
| `dynamodb:UpdateItem` | complete a record |
| `dynamodb:DeleteItem` | delete the record of a failed call |

### Required resources

#### DynamoDB table

| Configuration | Value |
|---|---|
| Partition key | `id`, a string |
| TTL attribute | `expiration` |

One table serves every idempotent function. With AWS SAM:

```yaml
IdempotencyTable:
  Type: AWS::DynamoDB::Table
  Properties:
    AttributeDefinitions:
      - AttributeName: id
        AttributeType: S
    KeySchema:
      - AttributeName: id
        KeyType: HASH
    TimeToLiveSpecification:
      AttributeName: expiration
      Enabled: true
    BillingMode: PAY_PER_REQUEST
```

Give the function the four permissions above on the table, for instance with SAM's
`DynamoDBCrudPolicy: {TableName: !Ref IdempotencyTable}`.

#### Limitations

* **Items are at most 400 KB**, the function's result included. Use a cache for larger results.
* **A first call costs 2 WCU**: a conditional `PutItem` to lock the key, an `UpdateItem` to complete it. A replay
  costs the failed `PutItem` only: DynamoDB returns the record with the failure.

#### Redis and Valkey

Amazon ElastiCache (Valkey or Redis OSS) and Amazon MemoryDB work, as any Redis-compatible cache. The function needs
[VPC access](https://docs.aws.amazon.com/lambda/latest/dg/configuration-vpc.html) to reach it.

### Making a handler idempotent

`idempotency.Idempotent` wraps a handler and returns one of the same type, ready for `lambda.Start`:

```go
package main

import (
	"context"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/alessandrolattao/powertools-lambda-go/idempotency"
	"github.com/alessandrolattao/powertools-lambda-go/idempotency/dynamodb"
)

type Payment struct {
	UserID    string  `json:"user_id"`
	ProductID string  `json:"product_id"`
	Amount    float64 `json:"amount"`
}

type PaymentResult struct {
	PaymentID string `json:"payment_id"`
	Message   string `json:"message"`
}

func handle(ctx context.Context, p Payment) (PaymentResult, error) {
	// Charge the customer, once.
	return PaymentResult{PaymentID: "pay-" + p.UserID, Message: "success"}, nil
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	store, err := dynamodb.FromConfig(cfg, os.Getenv("IDEMPOTENCY_TABLE"))
	if err != nil {
		panic(err)
	}
	lambda.Start(idempotency.Idempotent(handle, store))
}
```

### Making any function idempotent

The same call covers Python's `@idempotent_function`: the first argument after the context is the payload the key
comes from, and the result is what is stored. There is no output serializer to configure: the result is stored as
JSON and decoded back into its Go type on a replay, so it must survive a round trip through `encoding/json/v2`
(exported fields, no channels or functions).

```go
var processOrder = idempotency.Idempotent(func(ctx context.Context, o Order) (Receipt, error) {
	// ...
	return Receipt{OrderID: o.ID}, nil
}, store, idempotency.WithEventKeyJMESPath("order_id"))

func handle(ctx context.Context, event OrderEvent) (string, error) {
	receipt, err := processOrder(ctx, event.Order)
	if err != nil {
		return "", err
	}
	return receipt.OrderID, nil
}
```

The in-progress record expires with the invocation, whatever the function: its deadline comes from the context.

### Using the local cache

`WithLocalCache(true)` keeps completed records in memory as well, so that replays within one execution environment
do not read the store. `WithLocalCacheMaxItems` sets its capacity (256 by default), beyond which the least recently
used record goes. The cache belongs to one idempotent function in one execution environment; it is off by default,
since results may be large relative to the function's memory.

```go
handler := idempotency.Idempotent(handle, store,
	idempotency.WithLocalCache(true),
	idempotency.WithLocalCacheMaxItems(512))
```

### Choosing a payload subset

`WithEventKeyJMESPath` selects the part of the payload that identifies a request, with a JMESPath expression. A
request that differs elsewhere (a header, a timestamp) is still the same request:

```go
handler := idempotency.Idempotent(handle, store,
	idempotency.WithEventKeyJMESPath("powertools_json(body).[user_id, product_id]"))
```

The Powertools JMESPath functions are available (`powertools_json`, `powertools_base64`, `powertools_base64_gzip`),
as are the envelopes of the `jmespath` package. Decode JSON carried in strings with `powertools_json`: otherwise the
key depends on how the JSON is spaced. `WithJMESPathFunctions` adds functions of your own.

### Adjusting the expiration window

Records expire after an hour; `WithExpiresAfter` changes it, in whole seconds, with no upper limit. After it, the same
key runs the function again.

```go
handler := idempotency.Idempotent(handle, store, idempotency.WithExpiresAfter(24*time.Hour))
```

Records carry their own expiry, which is what counts: DynamoDB's TTL may take up to 48 hours to delete an item.

### Customizing the idempotency key generation

`WithKeyPrefix` replaces the names in the key, which makes it survive refactoring, and lets functions that should
share records agree on it. Changing the key generation leaves existing records behind.

```go
handler := idempotency.Idempotent(handle, store, idempotency.WithKeyPrefix("payments"))
```

Go names closures by position (`main.main.func1`): give a closure a prefix.

### Lambda timeouts

An in-progress record stores the deadline of its invocation. If the invocation times out, the next call with the same
key after the deadline runs the function again, instead of waiting for the record to expire. Outside Lambda, a context
without a deadline gives no in-progress expiry, and a warning.

### Handling errors

* When the function returns an error, the context's cancellation included, the in-progress record is deleted and the
  error returned, so that a retry runs the function again.
* When the function panics, the in-progress record is deleted and the panic goes on.
* Failures of the store are `*idempotency.PersistenceError` values, matching `idempotency.ErrPersistence`, with the
  operation that failed (`OpPut`, `OpGet`, `OpUpdate`, `OpDelete`). When deleting the record after an error of the
  function fails too, both errors are joined.

Errors handled outside the idempotent function keep its idempotency.

```go
result, err := handler(ctx, payment)
switch {
case errors.Is(err, idempotency.ErrAlreadyInProgress):
	// Another invocation is processing this payment: retry later.
case errors.Is(err, idempotency.ErrPayloadValidation):
	// Same key, different payload.
case errors.Is(err, idempotency.ErrPersistence):
	// The store failed.
}
```

### Persistence layers

#### DynamoDB

```go
store, err := dynamodb.New(client, "IdempotencyTable",
	dynamodb.WithKeyAttr("idempotency_key"),
	dynamodb.WithExpiryAttr("expires_at"),
	dynamodb.WithInProgressExpiryAttr("lock_expires_at"),
	dynamodb.WithStatusAttr("current_status"),
	dynamodb.WithDataAttr("result_data"),
	dynamodb.WithValidationKeyAttr("validation_key"))
```

`New` takes any client with the four methods it calls (`*dynamodb.Client` from aws-sdk-go-v2 has them), so that
endpoints, retries and middlewares are yours to configure; `FromConfig` builds one from an `aws.Config`.

| Option | Default | Attribute |
|---|---|---|
| `WithKeyAttr` | `id` | partition key: the idempotency key, unless there is a sort key |
| `WithExpiryAttr` | `expiration` | record expiry, epoch seconds |
| `WithInProgressExpiryAttr` | `in_progress_expiration` | in-progress expiry, epoch milliseconds |
| `WithStatusAttr` | `status` | `INPROGRESS` or `COMPLETED` |
| `WithDataAttr` | `data` | the result, as JSON |
| `WithValidationKeyAttr` | `validation` | the payload validation hash |
| `WithSortKeyAttr` | none | sort key of a table with a composite primary key |
| `WithStaticPKValue` | `idempotency#<function name>` | partition key value with a sort key |

##### Using a composite primary key

With `WithSortKeyAttr`, the idempotency key goes in the sort key, and every record's partition key is
`idempotency#<AWS_LAMBDA_FUNCTION_NAME>`, or the value of `WithStaticPKValue`:

```go
store, err := dynamodb.New(client, "IdempotencyTable",
	dynamodb.WithSortKeyAttr("sort_key"),
	dynamodb.WithStaticPKValue("idempotency#MyLambdaFunction"))
```

| id | sort_key | expiration | status | data |
|---|---|---|---|---|
| idempotency#MyLambdaFunction | orders#47c00b40b245c5389dfc3100ebb4b7a0 | 1636549553 | COMPLETED | `{"message":"success","user_id":12391}` |
| idempotency#MyLambdaFunction | orders#f091d2527ad1c78f05d54cc3f363be80 | 1636549585 | INPROGRESS | |

#### Redis and Valkey

`redis.New` takes any go-redis v9 client: `redis.UniversalClient`, so standalone, cluster and sentinel alike.
Configure TLS, timeouts and credentials on it:

```go
client := goredis.NewClient(&goredis.Options{
	Addr:      os.Getenv("CACHE_ENDPOINT"),
	Username:  "lambda",
	Password:  password, // from Secrets Manager, with the parameters utility
	TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
})
store, err := redis.New(client)
```

`FromConnection` takes the connection arguments of Python's `CachePersistenceLayer` instead, with TLS on unless
`DisableTLS` says otherwise:

```go
store, err := redis.FromConnection(redis.Connection{
	Host:     "my-cache.serverless.euw1.cache.amazonaws.com",
	Username: "lambda",
	Password: password,
	Mode:     redis.Cluster,
})
```

A record is a JSON object under its key, which expires with it:

```json
{"data": "{\"payment_id\":\"pay-1\"}", "status": "COMPLETED", "expiration": 1636549553, "validation": "..."}
```

| Option | Default | Field |
|---|---|---|
| `WithExpiryAttr` | `expiration` | record expiry, epoch seconds |
| `WithInProgressExpiryAttr` | `in_progress_expiration` | in-progress expiry, epoch milliseconds |
| `WithStatusAttr` | `status` | `INPROGRESS` or `COMPLETED` |
| `WithDataAttr` | `data` | the result, as JSON |
| `WithValidationKeyAttr` | `validation` | the payload validation hash |

Every command touches a single key, so clusters, ElastiCache Serverless and MemoryDB work. A record that no longer
holds its key but has not been evicted yet (an orphan) is replaced by the invocation that takes `<key>:lock` first;
the lock expires by itself after ten seconds.

### Common use cases

#### Batch processing

Make the record handler idempotent, with a key that identifies a record:

```go
var processMessage = idempotency.Idempotent(func(ctx context.Context, msg events.SQSMessage) (string, error) {
	// Process the message once.
	return msg.MessageId, nil
}, store, idempotency.WithEventKeyJMESPath("messageId"))

func handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	var response events.SQSEventResponse
	for _, msg := range event.Records {
		if _, err := processMessage(ctx, msg); err != nil {
			response.BatchItemFailures = append(response.BatchItemFailures,
				events.SQSBatchItemFailure{ItemIdentifier: msg.MessageId})
		}
	}
	return response, nil
}
```

A message whose first delivery is still running fails with `ErrAlreadyInProgress`, and comes back later.

### Idempotency request flow

1. The key is computed from the payload; a payload without a key [runs without idempotency](#making-the-idempotency-key-required).
2. With the local cache on, a cached completed record answers the call.
3. A conditional write creates the in-progress record. When it succeeds, the function runs: on success its result
   completes the record, on error (or panic) the record is deleted.
4. When a record already holds the key, it answers the call: a completed record replays its result (through the
   response hook, if any), an in-progress one fails the call with `ErrAlreadyInProgress`.
5. A record that changed between the write and the read (deleted, expired, timed out) starts the call over, three
   attempts in all, after which the call fails with `ErrInconsistentState`.

## Advanced

### Customizing the default behavior

| Option | Default | Python |
|---|---|---|
| `WithEventKeyJMESPath` | the whole payload | `event_key_jmespath` |
| `WithPayloadValidationJMESPath` | off | `payload_validation_jmespath` |
| `WithJMESPathFunctions` | the Powertools functions | `jmespath_options` |
| `WithKeyRequired` | `false` | `raise_on_no_idempotency_key` |
| `WithExpiresAfter` | one hour | `expires_after_seconds` |
| `WithLocalCache` | `false` | `use_local_cache` |
| `WithLocalCacheMaxItems` | 256 | `local_cache_max_items` |
| `WithHashFunction` | `md5` | `hash_function` |
| `WithHash` | | any `hash.Hash` |
| `WithKeyPrefix` | function names | `key_prefix` |
| `WithResponseHook` | none | `response_hook` |

`WithHashFunction` takes the names of Python's `hashlib`: `md5`, `sha1`, `sha224`, `sha256`, `sha384`, `sha512`,
`sha3_224`, `sha3_256`, `sha3_384`, `sha3_512`, `blake2b`, `blake2s`.

`Idempotent` checks its options when it is called and panics with an error matching `idempotency.ErrInvalidConfig`
when they cannot work (an expression that does not compile, an unknown hash name, a hook for another result type):
build idempotent functions in `main` or at package level, where such a mistake stops the function before it serves.

### Handling concurrent executions with the same payload

While a call runs, another call with the same key fails with an `*idempotency.AlreadyInProgressError`, which matches
`idempotency.ErrAlreadyInProgress`. Its result is not known yet, so the second call cannot be answered; it is safe to
retry.

### Payload validation

The key may leave out parts of the payload that should not change between retries, such as an amount. With
`WithPayloadValidationJMESPath`, their hash is stored with the record, and a call with the same key but different
values fails with `idempotency.ErrPayloadValidation` instead of replaying a result meant for another payload:

```go
handler := idempotency.Idempotent(handle, store,
	idempotency.WithEventKeyJMESPath("[user_id, product_id]"),
	idempotency.WithPayloadValidationJMESPath("amount"))
```

### Making the idempotency key required

A payload whose selection is empty (`null`, `""`, an empty object or array, an object or array of nulls) has no key.
By default, the function then runs without idempotency, reading and writing nothing, after a warning. With
`WithKeyRequired(true)`, the call fails with `idempotency.ErrNoIdempotencyKey` before the function runs. `0` and
`false` are keys.

### Customizing the AWS client

Build the DynamoDB client as you need, with a custom endpoint, retryer or middleware, and pass it to `dynamodb.New`:

```go
client := awsdynamodb.NewFromConfig(cfg, func(o *awsdynamodb.Options) {
	o.RetryMaxAttempts = 5
})
store, err := dynamodb.New(client, "IdempotencyTable")
```

### Bring your own persistent store

Implement `idempotency.Store`:

```go
type Store interface {
	PutRecord(ctx context.Context, record Record, now time.Time) error
	GetRecord(ctx context.Context, key string) (*Record, error)
	UpdateRecord(ctx context.Context, record Record) error
	DeleteRecord(ctx context.Context, key string) error
}
```

A store only moves records: keys, hashes, validation, the local cache and the status logic belong to `Idempotent`,
so one store serves functions with different options. Two rules keep idempotency intact:

* `PutRecord` writes the in-progress record unless a record with the same key `HoldsKey(now)`, and the check and the
  write are one atomic step. Otherwise it returns `idempotency.ErrItemAlreadyExists`, or an
  `*idempotency.ItemAlreadyExistsError` carrying the record when the store has it at hand, which saves a read.
* `GetRecord` reads the latest write, and returns `idempotency.ErrItemNotFound` when there is none.

`idempotency.MemoryStore` is a complete, small implementation to start from.

### Manipulating the idempotent response

`WithResponseHook` rewrites the result of a replay, for instance to tell the caller it is one. It receives the
decoded result and the record:

```go
handler := idempotency.Idempotent(handle, store, idempotency.WithResponseHook(
	func(ctx context.Context, response PaymentResult, record idempotency.Record) (PaymentResult, error) {
		response.Message += " (replayed)"
		return response, nil
	}))
```

#### Being a good citizen

1. The hook runs on replays only, never on a first execution or an error.
2. An error the hook returns is what the call returns: handle what can be handled inside it.
3. Keep it simple: it runs on every replay.

## Compatibility with other utilities

### Validation and envelopes

Make idempotency the innermost decorator. When a decorator outside it unwraps an envelope, the idempotent function
receives the unwrapped payload: write `WithEventKeyJMESPath` for that.

### Tracer

With the AWS SDK instrumented by OpenTelemetry, a first call shows a `PutItem` and an `UpdateItem`, a replay a failed
`PutItem` that returned the record.

### Sharing records with Python

A Go and a Python function compute the same key, and read each other's records, when:

1. they use the same explicit key prefix (the default one names the function in each language's way);
2. they use the same hash function and the same `event_key_jmespath`, over payloads with the same JSON shape. Go
   hashes its typed payload marshalled to JSON: fields its type does not declare are not part of it. Take
   `jsontext.Value` as the payload type to hash the raw event, exactly as Python sees it;
3. the selected values are strings, integers below 2^53, or non-integral numbers. JMESPath works on `float64` in Go,
   so an integral float (`10.0`) hashes as an integer (`10`), and larger integers lose precision: hash such values
   as strings. Numbers hashed from the whole payload, without an expression, keep their exact JSON form.

Tables and caches can always be shared between Go and Python functions with different keys.

## Testing your code

### Disabling the idempotency utility

Set `POWERTOOLS_IDEMPOTENCY_DISABLED` to a true value: the function is called directly, and a warning reminds that
this is for development only.

```go
func TestHandler(t *testing.T) {
	t.Setenv("POWERTOOLS_IDEMPOTENCY_DISABLED", "true")
	// ...
}
```

### Testing with an in-memory store

`idempotency.NewMemoryStore()` keeps records in memory, with the same semantics as the real stores:

```go
func TestPaymentIsChargedOnce(t *testing.T) {
	charges := 0
	charge := idempotency.Idempotent(func(context.Context, Payment) (PaymentResult, error) {
		charges++
		return PaymentResult{PaymentID: "pay-1"}, nil
	}, idempotency.NewMemoryStore())

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for range 2 {
		if _, err := charge(ctx, Payment{UserID: "u-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if charges != 1 {
		t.Fatalf("charged %d times", charges)
	}
}
```

### Testing with DynamoDB Local or an emulator

Point the client at the emulator:

```go
cfg, err := config.LoadDefaultConfig(ctx,
	config.WithRegion("us-east-1"),
	config.WithBaseEndpoint("http://localhost:8000"))
store, err := dynamodb.FromConfig(cfg, "IdempotencyTable")
```

`dynamodb.New` also takes a fake implementing the four methods of `dynamodb.Client`.

### Testing with Redis

[miniredis](https://github.com/alicebob/miniredis) runs an in-process Redis:

```go
server := miniredis.RunT(t)
store, err := redis.New(goredis.NewClient(&goredis.Options{Addr: server.Addr()}))
```

The integration tests of this repository run both stores against Floci and Valkey in containers.

## Differences from Python

* One function, `Idempotent`, covers `@idempotent` and `@idempotent_function`. Results are stored as JSON and decoded
  into the function's result type, so output serializers are not needed.
* The remaining time comes from the context's deadline; there is no `register_lambda_context`.
* Options belong to each idempotent function: Python keeps the first configuration a persistence layer sees.
* Fixed: stored results that are empty or zero (`{}`, `[]`, `0`, `""`, `false`) replay as such, not as `None`; records
  read back from a table with a composite key keep their idempotency key, so the local cache finds them; the cache
  layer keeps the validation hash of completed records, reads back a custom expiry field name, retries a key evicted
  between its `SET NX` and its `GET`, and never sends `EX 0`.
* The local cache is safe for concurrent invocations (Lambda Managed Instances).
* Not ported: durable function replays, the boto3 user agent tag, the deprecated `Redis*` names.
