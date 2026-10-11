package eventsource

import (
	"errors"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

// Errors of the response helpers.
var (
	// ErrEmptyResponse reports a response with no record, which Firehose
	// and S3 Batch Operations reject.
	ErrEmptyResponse = errors.New("response has no record")
	// ErrInvalidResult reports a result or result code the service does not
	// know.
	ErrInvalidResult = errors.New("invalid result")
)

// KinesisFirehoseResponse is the response of an Amazon Data Firehose
// transformation function. Unlike events.KinesisFirehoseResponse, a record
// without metadata leaves it out, as Powertools Python does, instead of
// sending empty partition keys and on-the-fly metadata.
type KinesisFirehoseResponse struct {
	Records []KinesisFirehoseResponseRecord `json:"records"`
}

// KinesisFirehoseResponseRecord is a transformed record:
// events.KinesisFirehoseResponseRecord with optional metadata. Result is
// events.KinesisFirehoseTransformedStateOk, Dropped or ProcessingFailed;
// Data is the transformed data, which the JSON encoder base64-encodes:
//
//	eventsource.KinesisFirehoseResponseRecord{
//		RecordID: record.RecordID,
//		Result:   events.KinesisFirehoseTransformedStateOk,
//		Data:     transformed,
//	}
type KinesisFirehoseResponseRecord struct {
	events.KinesisFirehoseResponseRecord

	Metadata *KinesisFirehoseResponseRecordMetadata `json:"metadata,omitempty"`
}

// KinesisFirehoseResponseRecordMetadata is the metadata of a transformed
// record: the partition keys of dynamic partitioning, and the routing of an
// Apache Iceberg destination.
type KinesisFirehoseResponseRecordMetadata struct {
	PartitionKeys map[string]string                                `json:"partitionKeys,omitempty"`
	OTFMetadata   *events.KinesisFirehoseResponseRecordOTFMetadata `json:"otfMetadata,omitempty"`
}

// Add appends records to the response.
func (r *KinesisFirehoseResponse) Add(records ...KinesisFirehoseResponseRecord) {
	r.Records = append(r.Records, records...)
}

// Validate reports what Firehose would reject: no record, or a result other
// than Ok, Dropped and ProcessingFailed. Powertools Python raises on the
// first and only warns on the second.
func (r *KinesisFirehoseResponse) Validate() error {
	if len(r.Records) == 0 {
		return ErrEmptyResponse
	}
	for _, record := range r.Records {
		switch record.Result {
		case events.KinesisFirehoseTransformedStateOk, events.KinesisFirehoseTransformedStateDropped,
			events.KinesisFirehoseTransformedStateProcessingFailed:
		default:
			return fmt.Errorf("%w %q for record %q", ErrInvalidResult, record.Result, record.RecordID)
		}
	}
	return nil
}
