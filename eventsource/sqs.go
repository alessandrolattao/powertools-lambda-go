package eventsource

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// ErrInvalidQueueARN reports an event source ARN that is not an SQS queue
// ARN.
var ErrInvalidQueueARN = errors.New("invalid SQS queue ARN")

// The names of the SQS system attributes a message carries.
const (
	SQSAttributeAWSTraceHeader                   = "AWSTraceHeader"
	SQSAttributeApproximateReceiveCount          = "ApproximateReceiveCount"
	SQSAttributeSentTimestamp                    = "SentTimestamp"
	SQSAttributeSenderID                         = "SenderId"
	SQSAttributeApproximateFirstReceiveTimestamp = "ApproximateFirstReceiveTimestamp"
	SQSAttributeSequenceNumber                   = "SequenceNumber"
	SQSAttributeMessageGroupID                   = "MessageGroupId"
	SQSAttributeMessageDeduplicationID           = "MessageDeduplicationId"
	SQSAttributeDeadLetterQueueSourceARN         = "DeadLetterQueueSourceArn"
)

// SQSSystemAttributes are the system attributes of an SQS message, parsed.
// FIFO members are empty for standard queues.
type SQSSystemAttributes struct {
	AWSTraceHeader                   string
	ApproximateReceiveCount          int
	SentTimestamp                    time.Time
	SenderID                         string
	ApproximateFirstReceiveTimestamp time.Time
	SequenceNumber                   string
	MessageGroupID                   string
	MessageDeduplicationID           string
	// DeadLetterQueueSourceARN is set when the function reads a dead-letter
	// queue: the queue the message was moved from.
	DeadLetterQueueSourceARN string
}

// ParseSQSAttributes parses the system attributes of a message: the counts
// as integers, the timestamps (milliseconds since the epoch) as times.
func ParseSQSAttributes(m events.SQSMessage) (SQSSystemAttributes, error) {
	attrs := SQSSystemAttributes{
		AWSTraceHeader:           m.Attributes[SQSAttributeAWSTraceHeader],
		SenderID:                 m.Attributes[SQSAttributeSenderID],
		SequenceNumber:           m.Attributes[SQSAttributeSequenceNumber],
		MessageGroupID:           m.Attributes[SQSAttributeMessageGroupID],
		MessageDeduplicationID:   m.Attributes[SQSAttributeMessageDeduplicationID],
		DeadLetterQueueSourceARN: m.Attributes[SQSAttributeDeadLetterQueueSourceARN],
	}
	if raw, ok := m.Attributes[SQSAttributeApproximateReceiveCount]; ok {
		count, err := strconv.Atoi(raw)
		if err != nil {
			return attrs, fmt.Errorf("parse %s %q: %w", SQSAttributeApproximateReceiveCount, raw, err)
		}
		attrs.ApproximateReceiveCount = count
	}
	var err error
	if attrs.SentTimestamp, err = millis(m.Attributes, SQSAttributeSentTimestamp); err != nil {
		return attrs, err
	}
	if attrs.ApproximateFirstReceiveTimestamp, err = millis(m.Attributes, SQSAttributeApproximateFirstReceiveTimestamp); err != nil {
		return attrs, err
	}
	return attrs, nil
}

func millis(attrs map[string]string, name string) (time.Time, error) {
	raw, ok := attrs[name]
	if !ok {
		return time.Time{}, nil
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s %q: %w", name, raw, err)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// dnsSuffixes maps the AWS partitions to the DNS suffix of their service
// endpoints.
var dnsSuffixes = map[string]string{
	"aws":        "amazonaws.com",
	"aws-cn":     "amazonaws.com.cn",
	"aws-us-gov": "amazonaws.com",
	"aws-iso":    "c2s.ic.gov",
	"aws-iso-b":  "sc2s.sgov.gov",
	"aws-iso-e":  "cloud.adc-e.uk",
	"aws-iso-f":  "csp.hci.ic.gov",
	"aws-eusc":   "amazonaws.eu",
}

// SQSQueueURL returns the URL of the queue an event source ARN names
// (arn:aws:sqs:us-east-1:123456789012:orders). Unlike Powertools Python's
// queue_url, which always writes amazonaws.com, it uses the DNS suffix of
// the ARN's partition, so that China and isolated regions work.
func SQSQueueURL(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sqs" || parts[3] == "" || parts[4] == "" || parts[5] == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidQueueARN, arn)
	}
	suffix, ok := dnsSuffixes[parts[1]]
	if !ok {
		suffix = dnsSuffixes[defaultPartition]
	}
	return "https://sqs." + parts[3] + "." + suffix + "/" + parts[4] + "/" + parts[5], nil
}

// NestedS3Event decodes the S3 event notification an S3 bucket sent to the
// queue, which is the message body.
func NestedS3Event(m events.SQSMessage) (events.S3Event, error) {
	var out events.S3Event
	if err := json.Unmarshal([]byte(m.Body), &out); err != nil {
		return out, fmt.Errorf("decode the S3 event in the message body: %w", err)
	}
	return out, nil
}

// NestedSNSMessage decodes the SNS notification an SNS topic delivered to
// the queue (without raw message delivery), which is the message body.
// SNS spells two members differently there (SigningCertURL, UnsubscribeURL)
// than in the events it sends to Lambda; both spellings decode.
func NestedSNSMessage(m events.SQSMessage) (events.SNSEntity, error) {
	var out events.SNSEntity
	if err := json.Unmarshal([]byte(m.Body), &out, json.MatchCaseInsensitiveNames(true)); err != nil {
		return out, fmt.Errorf("decode the SNS notification in the message body: %w", err)
	}
	return out, nil
}
