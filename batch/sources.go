package batch

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// KafkaEventResponse is the partial batch response of a Kafka event (Amazon
// MSK or self-managed Apache Kafka), which aws-lambda-go does not define.
type KafkaEventResponse struct {
	BatchItemFailures []KafkaBatchItemFailure `json:"batchItemFailures"`
}

// KafkaBatchItemFailure is a Kafka record that failed processing.
type KafkaBatchItemFailure struct {
	ItemIdentifier KafkaItemIdentifier `json:"itemIdentifier"`
}

// KafkaItemIdentifier identifies a Kafka record: Lambda wants the topic and
// the partition joined as "<topic>-<partition>" under the key "partition",
// and the record's offset.
type KafkaItemIdentifier struct {
	Partition string `json:"partition"`
	Offset    int64  `json:"offset"`
}

// Handler turns a record handler into a Lambda handler for the event that
// process takes: Powertools Python's process_partial_response as a
// decorator.
//
//	lambda.Start(batch.Handler(batch.ProcessSQS, handleMessage))
func Handler[E, R, Resp any](process ProcessFunc[E, R, Resp], handler RecordHandler[R], opts ...Option) func(ctx context.Context, event E) (Resp, error) {
	return func(ctx context.Context, event E) (Resp, error) {
		return process(ctx, event, handler, opts...)
	}
}

// ProcessSQS processes the messages of a standard SQS queue: every message
// goes to handler, and the response lists those that failed, by message
// id, so that only they return to the queue. The event source mapping must
// enable ReportBatchItemFailures.
//
// When every message fails, it also returns a *ProcessingError (see
// WithRaiseOnEntireBatchFailure). An event without messages is an
// ErrUnexpectedBatchType error.
func ProcessSQS(ctx context.Context, event events.SQSEvent, handler RecordHandler[events.SQSMessage], opts ...Option) (events.SQSEventResponse, error) {
	failures, err := partial(ctx, event.Records, handler, opts, nil, sqsFailure)
	return events.SQSEventResponse{BatchItemFailures: failures}, err
}

// ProcessSQSFIFO processes the messages of an SQS FIFO queue, in order. At
// the first failure it stops: the failed message and every one after it are
// reported, the latter failing with ErrFIFOShortCircuit without reaching
// handler, so that the queue delivers them again in the same order.
// WithSkipGroupOnError limits that to the failed message's group.
//
// It never processes messages concurrently, whatever WithConcurrency says.
func ProcessSQSFIFO(ctx context.Context, event events.SQSEvent, handler RecordHandler[events.SQSMessage], opts ...Option) (events.SQSEventResponse, error) {
	failures, err := partial(ctx, event.Records, handler, opts, messageGroupID, sqsFailure)
	return events.SQSEventResponse{BatchItemFailures: failures}, err
}

func messageGroupID(msg events.SQSMessage) string {
	return msg.Attributes["MessageGroupId"]
}

func sqsFailure(msg events.SQSMessage) events.SQSBatchItemFailure {
	return events.SQSBatchItemFailure{ItemIdentifier: msg.MessageId}
}

// ProcessKinesis processes the records of a Kinesis data stream: every
// record goes to handler, and the response lists those that failed, by
// sequence number. Lambda checkpoints the shard at the lowest one and
// retries from there.
//
// Batch failures are as for ProcessSQS.
func ProcessKinesis(ctx context.Context, event events.KinesisEvent, handler RecordHandler[events.KinesisEventRecord], opts ...Option) (events.KinesisEventResponse, error) {
	failures, err := partial(ctx, event.Records, handler, opts, nil,
		func(rec events.KinesisEventRecord) events.KinesisBatchItemFailure {
			return events.KinesisBatchItemFailure{ItemIdentifier: rec.Kinesis.SequenceNumber}
		})
	return events.KinesisEventResponse{BatchItemFailures: failures}, err
}

// ProcessDynamoDB processes the records of a DynamoDB stream: every record
// goes to handler, and the response lists those that failed, by sequence
// number. Lambda checkpoints the shard at the lowest one and retries from
// there.
//
// Batch failures are as for ProcessSQS.
func ProcessDynamoDB(ctx context.Context, event events.DynamoDBEvent, handler RecordHandler[events.DynamoDBEventRecord], opts ...Option) (events.DynamoDBEventResponse, error) {
	failures, err := partial(ctx, event.Records, handler, opts, nil,
		func(rec events.DynamoDBEventRecord) events.DynamoDBBatchItemFailure {
			return events.DynamoDBBatchItemFailure{ItemIdentifier: rec.Change.SequenceNumber}
		})
	return events.DynamoDBEventResponse{BatchItemFailures: failures}, err
}

// ProcessKafka processes the records of an Amazon MSK or self-managed Apache
// Kafka event: every record goes to handler, and the response lists those
// that failed, by topic, partition and offset. Lambda supports it on event
// source mappings in provisioned mode.
//
// The event groups records by topic and partition in a map, which has no
// order in Go: the records are processed topic by topic, partition by
// partition in ascending order, each partition's records in the order of
// the event. Batch failures are as for ProcessSQS.
func ProcessKafka(ctx context.Context, event events.KafkaEvent, handler RecordHandler[events.KafkaRecord], opts ...Option) (KafkaEventResponse, error) {
	failures, err := partial(ctx, kafkaRecords(event), handler, opts, nil,
		func(rec events.KafkaRecord) KafkaBatchItemFailure {
			return KafkaBatchItemFailure{ItemIdentifier: KafkaItemIdentifier{
				Partition: rec.Topic + "-" + strconv.FormatInt(rec.Partition, 10),
				Offset:    rec.Offset,
			}}
		})
	return KafkaEventResponse{BatchItemFailures: failures}, err
}

// partial processes records and returns the failure of each failed one, as
// failure describes it to Lambda.
func partial[R, F any](ctx context.Context, records []R, handler RecordHandler[R], opts []Option,
	group func(R) string, failure func(R) F,
) ([]F, error) {
	cfg := newConfig(opts)
	errs, err := process(ctx, records, handler, &cfg, group)
	if err != nil {
		return nil, err
	}
	return respond(ctx, records, errs, &cfg, failure)
}

// kafkaRecords flattens the records of event in a stable order.
func kafkaRecords(event events.KafkaEvent) []events.KafkaRecord {
	keys := slices.SortedFunc(maps.Keys(event.Records), compareTopicPartition)
	total := 0
	for _, key := range keys {
		total += len(event.Records[key])
	}
	records := make([]events.KafkaRecord, 0, total)
	for _, key := range keys {
		records = append(records, event.Records[key]...)
	}
	return records
}

// compareTopicPartition orders "<topic>-<partition>" keys by topic, then by
// partition number, so that partition 10 comes after partition 9.
func compareTopicPartition(a, b string) int {
	topicA, partitionA := splitTopicPartition(a)
	topicB, partitionB := splitTopicPartition(b)
	return cmp.Or(cmp.Compare(topicA, topicB), cmp.Compare(partitionA, partitionB), cmp.Compare(a, b))
}

// splitTopicPartition splits a records key at its last dash; a key that
// does not end with a partition number is all topic.
func splitTopicPartition(key string) (topic string, partition int64) {
	before, after, found := strings.CutLast(key, "-")
	if !found {
		return key, -1
	}
	partition, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		return key, -1
	}
	return before, partition
}
