// Package batch processes the records of a batch event one at a time and
// reports to Lambda only those that failed, so that only they are retried:
// the partial batch response of Amazon SQS (standard and FIFO queues),
// Kinesis Data Streams, DynamoDB Streams, and Amazon MSK or self-managed
// Apache Kafka. It is the Go side of Powertools for AWS Lambda (Python)'s
// batch processing utility.
//
// # Getting started
//
// The event source mapping must enable ReportBatchItemFailures (for Kafka,
// in provisioned mode). Write a record handler, which returns an error for
// a record that must be retried, and hand it to the Process function of the
// event source, or turn it into the Lambda handler with Handler:
//
//	func handleMessage(ctx context.Context, msg events.SQSMessage) error {
//		return orders.Save(ctx, msg.Body)
//	}
//
//	func main() {
//		lambda.Start(batch.Handler(batch.ProcessSQS, handleMessage))
//	}
//
// Every record reaches the handler, even after some failed. The response
// lists the failed ones by the identifier Lambda expects: the message id
// for SQS, the sequence number for Kinesis and DynamoDB, the topic,
// partition and offset for Kafka. When every record succeeds the list is
// empty.
//
//	Source                 Process function   Response
//	SQS standard queue     ProcessSQS         events.SQSEventResponse
//	SQS FIFO queue         ProcessSQSFIFO     events.SQSEventResponse
//	Kinesis Data Streams   ProcessKinesis     events.KinesisEventResponse
//	DynamoDB Streams       ProcessDynamoDB    events.DynamoDBEventResponse
//	Amazon MSK, Kafka      ProcessKafka       KafkaEventResponse
//
// # Failures
//
// A record fails when its handler returns an error or panics; the panic is
// recovered into a *PanicError carrying the panic value and the stack, so
// that one bad record does not take the batch down. When the invocation's
// context ends (its deadline passes, or it is cancelled), processing stops:
// the records that had not started fail with ErrNotProcessed, without
// reaching the handler.
//
// When every record fails, the Process function returns a *ProcessingError
// next to the response. Returned from the handler, it fails the invocation:
// the error shows in the function's metrics and Lambda retries the whole
// batch. WithRaiseOnEntireBatchFailure(false) returns the response alone
// instead, listing every record, for functions that use errors as flow
// control and would otherwise see Lambda scale their SQS pollers down. An
// event without records fails with ErrUnexpectedBatchType.
//
// # SQS FIFO queues
//
// ProcessSQSFIFO keeps the queue's order: it processes the messages one
// after the other and, at the first failure, fails all the messages that
// follow with ErrFIFOShortCircuit without processing them, so that the
// queue redelivers them in order. With WithSkipGroupOnError, only the
// following messages of the failed message's group fail
// (ErrFIFOGroupShortCircuit); the other groups go on. A message without a
// group never stops the others.
//
// # Typed records
//
// SQSJSON, KinesisJSON, KafkaJSON and DynamoDBImages adapt a handler that
// takes the record's payload decoded into a Go type (the order, not the SQS
// message carrying it), the way Powertools Python decodes records into
// Pydantic models. A payload that does not decode, or whose Validate method
// rejects it, fails its record with ErrDecode: a poison pill is reported
// like any other failure and does not stop the batch.
//
//	func handleOrder(ctx context.Context, order Order) error { ... }
//
//	lambda.Start(batch.Handler(batch.ProcessSQS, batch.SQSJSON(handleOrder)))
//
// # Options
//
//   - WithRaiseOnEntireBatchFailure: whether a batch whose records all
//     failed fails the invocation (default true).
//   - WithConcurrency: how many records are processed at the same time
//     (default 1, one after the other). Ignored by ProcessSQSFIFO.
//   - WithSkipGroupOnError: ProcessSQSFIFO fails only the failed message's
//     group.
//   - WithLogger: a logger that receives a warning for every record whose
//     handler failed.
//   - WithBeforeRecord, WithAfterRecord, WithOnFailure: hooks around each
//     record, which take the record type of the batch (events.SQSMessage
//     for ProcessSQS). A hook of another type makes the Process function
//     fail with ErrHookRecordType before any record runs.
//
// The package reads no environment variable. Internal diagnostics go to
// standard error when POWERTOOLS_DEBUG is on.
//
// # Concurrency
//
// The Process functions keep no state between calls: concurrent
// invocations (Lambda Managed Instances) can share options and handlers.
// With WithConcurrency above 1, the handler and the hooks run in several
// goroutines at once and must be safe for it; the response keeps the order
// of the batch.
//
// # Differences from Powertools Python
//
//   - There are no processor objects, context managers or event type enum:
//     each event source has its Process function, Handler is
//     process_partial_response as a decorator, and the deprecated
//     batch_processor decorators are not ported.
//   - Subclassing BatchProcessor to override success_handler and
//     failure_handler, or BasePartialProcessor to write a processor, becomes
//     the record hooks. WithAfterRecord and WithOnFailure receive the same
//     typed record, where Python passes the raw record to success_handler
//     and the typed one to failure_handler. Work that a custom processor does
//     once the batch is over (storing the successful records, say) goes
//     after the Process function returns, with a hook collecting what it
//     needs. Accessing the processed messages works the same way.
//   - AsyncBatchProcessor becomes WithConcurrency, with a bound Python does
//     not have; asyncio.gather starts every record at once.
//   - The record handler returns only an error: the results Python collects
//     from the handler have no use in the response.
//   - The Lambda context is not injected by parameter name: it travels in
//     the context.Context (lambdacontext.FromContext).
//   - Pydantic models become the typed adapters. Validation runs when the
//     decoded type has a Validate method.
//   - ProcessSQSFIFO honors WithRaiseOnEntireBatchFailure, which Python's
//     SqsFifoPartialProcessor does not expose.
//   - A panic fails its record, where Python only catches Exception, and
//     the end of the invocation's context stops the batch.
//   - Kafka failures use the key "partition", as Lambda and Powertools
//     Python's code do; the upstream documentation's sample response shows
//     "topic-partition", which Lambda rejects. Records are processed in
//     topic and partition order, since aws-lambda-go hands them over in a
//     map, where Python keeps the order of the JSON event.
//   - Kinesis and DynamoDB time windows (tumbling windows) are not
//     supported, as in Python.
package batch
