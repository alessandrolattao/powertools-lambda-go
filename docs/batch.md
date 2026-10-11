# Batch processing

The `batch` package handles partial failures when processing batches from
Amazon SQS, Amazon Kinesis Data Streams, Amazon DynamoDB Streams, and Amazon
MSK or self-managed Apache Kafka.

```go
import "github.com/alessandrolattao/powertools-lambda-go/batch"
```

## Key features

- Reports batch item failures, so that only the failed records are retried
- A simple interface to process each record: one function per record
- Typed records: decode the body of each record into your own Go type
- Bounded concurrent processing, and in-order processing for SQS FIFO queues
- Hooks around each record, for metrics and error tracking

## Background

When an SQS queue, a Kinesis or DynamoDB stream, or a Kafka topic triggers a
Lambda function, the function receives a batch of records. If it fails to
process any of them, the whole batch returns to the queue or stream and is
retried until the function succeeds, the records reach their maximum retries,
or they expire.

With `ReportBatchItemFailures` enabled on the event source mapping, the
function can instead report which records failed:

- **SQS queues**: only the reported messages return to the queue; the others
  are deleted.
- **Kinesis and DynamoDB streams**: the stream checkpoints at the lowest
  reported sequence number and retries from there.
- **Kafka**: failed records are identified by topic, partition and offset, and
  only they are retried.

!!! warning
    This utility lowers the chance of processing a record more than once; it
    does not rule it out. Make the processing idempotent wherever you can (see
    the idempotency utility).

## Getting started

Two things are needed: the event source mapping must enable
`ReportBatchItemFailures` (in your infrastructure as code), and the function
must return the partial batch response, which this package builds.

### Required resources

No IAM permission beyond what the event source needs. In AWS SAM:

```yaml
Events:
  Batch:
    Type: SQS
    Properties:
      Queue: !GetAtt SampleQueue.Arn
      FunctionResponseTypes:
        - ReportBatchItemFailures
```

For Kafka, `ReportBatchItemFailures` is available on event source mappings in
provisioned mode.

### Processing messages from SQS

Write a record handler, which returns an error for a record that must be
retried, and pass it to `ProcessSQS`, or turn it into the Lambda handler with
`Handler`:

```go
func handleMessage(ctx context.Context, msg events.SQSMessage) error {
	if msg.Body == "" {
		return errors.New("empty message")
	}
	return nil // processed: SQS deletes the message
}

func main() {
	lambda.Start(batch.Handler(batch.ProcessSQS, handleMessage))
}
```

`ProcessSQS` is the same thing, called from your own handler:

```go
func handler(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	return batch.ProcessSQS(ctx, event, handleMessage)
}
```

The second message below failed, so the response reports its message id:

```json
{"batchItemFailures":[{"itemIdentifier":"244fc6b4"}]}
```

#### FIFO queues

`ProcessSQSFIFO` processes the messages of an SQS FIFO queue in order. At the
first failure it stops and reports the failed message and all those after it,
which fail with `ErrFIFOShortCircuit` without being processed, so that the
queue delivers them again in the same order.

`WithSkipGroupOnError` limits that to the message group of the failed
message: the following messages of the same group are reported
(`ErrFIFOGroupShortCircuit`), the other groups go on.

```go
handle := func(_ context.Context, msg events.SQSMessage) error {
	if msg.Body == "bad" {
		return errors.New("cannot process")
	}
	fmt.Println("processed", msg.MessageId)
	return nil
}
resp, err := batch.ProcessSQSFIFO(ctx, event, handle, batch.WithSkipGroupOnError())
```

### Processing messages from Kinesis

`ProcessKinesis` reports the failed records by sequence number.
aws-lambda-go has already decoded the record data from base64.

```go
handle := func(_ context.Context, rec events.KinesisEventRecord) error {
	fmt.Printf("%s\n", rec.Kinesis.Data)
	return nil
}
lambda.Start(batch.Handler(batch.ProcessKinesis, handle))
```

### Processing messages from DynamoDB

`ProcessDynamoDB` reports the failed records by sequence number.

```go
handle := func(_ context.Context, rec events.DynamoDBEventRecord) error {
	if rec.EventName == "REMOVE" {
		return nil
	}
	fmt.Println("changed", rec.Change.Keys["pk"].String())
	return nil
}
lambda.Start(batch.Handler(batch.ProcessDynamoDB, handle))
```

### Processing messages from Kafka

`ProcessKafka` works with Amazon MSK and self-managed Apache Kafka. It returns
a `KafkaEventResponse`, which aws-lambda-go does not define, identifying each
failed record by topic and partition (`"<topic>-<partition>"`, under the key
`partition`) and offset:

```go
handle := func(_ context.Context, rec events.KafkaRecord) error {
	if rec.Offset == 16 {
		return errors.New("poison pill")
	}
	return nil
}
resp, err := batch.ProcessKafka(ctx, event, handle)
```

```json
{"batchItemFailures":[{"itemIdentifier":{"partition":"mytopic-0","offset":16}}]}
```

The event groups records by topic and partition in a map: they are processed
topic by topic, partition by partition in ascending order. To decode Avro or
Protobuf values, use the Kafka utility within the record handler.

### Error handling

Any error the record handler returns marks the record as failed, and the batch
goes on. A panic does too: it is recovered into a `*PanicError` with the panic
value and the stack trace, so that one bad record does not take down the
whole batch.

### Partial failure mechanics

Every record goes to the record handler, even after some failed. Then:

- **All records succeeded**: the response has an empty list,
  `{"batchItemFailures":[]}`.
- **Some records failed**: the response lists them.
- **All records failed**: the Process function also returns a
  `*ProcessingError` holding every record's error. Returned from the handler,
  it fails the invocation, so that the failure shows in your metrics and
  Lambda retries the whole batch.

When the invocation's context ends (its deadline passes, or it is cancelled),
processing stops: the records that had not started fail with
`ErrNotProcessed`, without reaching the handler.

An event without records fails with `ErrUnexpectedBatchType`: it usually means
the function receives events from another source than it expects.

### Processing messages concurrently

`WithConcurrency(n)` processes up to `n` records at the same time, each in its
own goroutine. Use it when the records do not depend on each other's order:
saving loyalty points one after the other would not need it, calling a slow
API for each record would. The response keeps the order of the batch.
`ProcessSQSFIFO` ignores it: a FIFO queue is always processed in order.

```go
lambda.Start(batch.Handler(batch.ProcessSQS, handleMessage, batch.WithConcurrency(8)))
```

With concurrency, the record handler and the hooks run in several goroutines
at once and must be safe for concurrent use.

## Advanced

### Typed records

Powertools Python decodes records into Pydantic models. Here, the adapters
`SQSJSON`, `KinesisJSON`, `KafkaJSON` and `DynamoDBImages` decode the payload
of each record into your Go type before calling your handler:

```go
type Order struct {
	Item string `json:"item"`
}
handle := func(_ context.Context, order Order) error {
	fmt.Println("ordered", order.Item)
	return nil
}
resp, err := batch.ProcessSQS(ctx, event, batch.SQSJSON(handle))
```

A payload that does not decode fails its record with `ErrDecode`, and the
batch goes on: a poison pill is reported like any other failure. Decoding uses
`encoding/json/v2` and is lenient: unknown fields are ignored, missing ones
stay zero. When the type has a `Validate() error` method, it runs after
decoding, and its error fails the record too.

`DynamoDBImages` decodes both images of a stream record, `nil` when the record
lacks one (no new image in a `REMOVE`, no old one in an `INSERT`):

```go
type Product struct {
	SKU   string `json:"sku"`
	Stock int    `json:"stock"`
}
handle := func(_ context.Context, newImage, oldImage *Product) error {
	switch {
	case oldImage == nil:
		fmt.Println("new product", newImage.SKU, newImage.Stock)
	case newImage == nil:
		fmt.Println("removed product", oldImage.SKU)
	default:
		fmt.Println("stock of", newImage.SKU, "went from", oldImage.Stock, "to", newImage.Stock)
	}
	return nil
}
_, err := batch.ProcessDynamoDB(ctx, event, batch.DynamoDBImages(handle))
```

DynamoDB attributes become JSON first: numbers stay numbers (with all their
digits), binary values become base64 strings, sets become arrays.

### Working with full batch failures

By default a batch whose records all failed fails the invocation. When
batches are small, or errors are part of the flow, that may make Lambda scale
the SQS pollers down. `WithRaiseOnEntireBatchFailure(false)` returns the
response alone instead, listing every record:

```go
failing := func(context.Context, events.SQSMessage) error { return errors.New("downstream unavailable") }

_, err := batch.ProcessSQS(ctx, event, failing)
batchFailure, ok := errors.AsType[*batch.ProcessingError](err)
fmt.Println("default:", ok, len(batchFailure.Errors))

resp, err := batch.ProcessSQS(ctx, event, failing, batch.WithRaiseOnEntireBatchFailure(false))
fmt.Println("disabled:", err, len(resp.BatchItemFailures))
```

### Accessing the Lambda context

The record handler receives the invocation's context:
`lambdacontext.FromContext(ctx)` returns the Lambda context, and
`ctx.Deadline()` tells how much time is left.

### Extending the processing with hooks

Powertools Python lets you subclass `BatchProcessor` and override
`success_handler` and `failure_handler`, or write a processor from
`BasePartialProcessor`. Here, hooks do the same:

- `WithBeforeRecord` runs right before the handler receives a record;
- `WithAfterRecord` runs once a record's outcome is known (`err` is `nil` for
  a success), for every record, including those skipped without processing;
- `WithOnFailure` runs for every failed record: send the error to your error
  tracker, or count it in a metric.

Hooks take the record type of the batch (`events.SQSMessage` for
`ProcessSQS`). A hook of another type makes the Process function fail with
`ErrHookRecordType` before any record runs.

What a custom processor does once the batch is over, such as storing the
successful records at once, goes after the Process function returns, with a
hook collecting what it needs:

```go
var (
	mu        sync.Mutex
	succeeded []string
)
collect := batch.WithAfterRecord(func(_ context.Context, msg events.SQSMessage, err error) {
	if err == nil {
		mu.Lock()
		defer mu.Unlock()
		succeeded = append(succeeded, msg.MessageId)
	}
})
_, err := batch.ProcessSQS(ctx, event, handleMessage, collect, batch.WithConcurrency(2))
// Write the succeeded records at once here.
```

`WithLogger(logger)` writes a warning to your logger for every record whose
handler failed.

### Caveats

#### Tracing large batches

When you trace each record (wrapping the record handler in a span), avoid
recording what it returns: with large batches the trace data may exceed the
X-Ray limits.

## Testing your code

There are no external calls: build an event and call your handler.

```go
func TestHandler(t *testing.T) {
	event := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "059f36b4", Body: "hello"},
		{MessageId: "244fc6b4", Body: ""},
	}}
	resp, err := batch.ProcessSQS(t.Context(), event, handleMessage)
	require.NoError(t, err)
	assert.Equal(t, []events.SQSBatchItemFailure{{ItemIdentifier: "244fc6b4"}}, resp.BatchItemFailures)
}
```

## Differences from Powertools Python

- No processor objects or context managers: one Process function per event
  source, and `Handler` as `process_partial_response`. The deprecated
  `batch_processor` decorators are not ported.
- `AsyncBatchProcessor` is `WithConcurrency`, with a bound.
- The record handler returns only an error.
- `WithAfterRecord` and `WithOnFailure` receive the same typed record, where
  Python passes the raw record to `success_handler` only.
- `ProcessSQSFIFO` accepts `WithRaiseOnEntireBatchFailure`.
- Panics fail their record; the end of the context stops the batch.
- Kafka responses use the key `partition`, as Lambda requires (the upstream
  documentation's sample shows `topic-partition`).
