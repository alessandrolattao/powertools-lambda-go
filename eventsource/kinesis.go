package eventsource

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-lambda-go/events"
)

// MaxDecompressedSize bounds what Decompress inflates, so that a small
// malicious payload cannot exhaust the function's memory.
const MaxDecompressedSize = 128 << 20

// Errors of the decompression helpers.
var (
	// ErrTooLarge reports compressed data that inflates beyond
	// MaxDecompressedSize.
	ErrTooLarge = errors.New("decompressed data is too large")
	// ErrNotCompressed reports data that is neither gzip nor zlib.
	ErrNotCompressed = errors.New("data is neither gzip nor zlib compressed")
)

// Decompress inflates gzip or zlib data, recognizing the format from its
// header as Python's zlib.decompress(data, MAX_WBITS | 32) does. CloudWatch
// Logs subscriptions, and producers that compress Kinesis records, use it.
func Decompress(data []byte) ([]byte, error) {
	var (
		reader io.ReadCloser
		err    error
	)
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		reader, err = gzip.NewReader(bytes.NewReader(data))
	case len(data) >= 2 && data[0]&0x0f == 8 && (uint16(data[0])<<8|uint16(data[1]))%31 == 0:
		reader, err = zlib.NewReader(bytes.NewReader(data))
	default:
		return nil, ErrNotCompressed
	}
	if err != nil {
		return nil, fmt.Errorf("open the compressed stream: %w", err)
	}
	defer func() { _ = reader.Close() }() //nolint:errcheck // a read-only decompressor has nothing to report on close
	decoded, err := io.ReadAll(io.LimitReader(reader, MaxDecompressedSize+1))
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	if len(decoded) > MaxDecompressedSize {
		return nil, ErrTooLarge
	}
	return decoded, nil
}

// ExtractCloudWatchLogs decodes the CloudWatch Logs subscription payloads a
// Kinesis stream delivered, one per record, in order.
func ExtractCloudWatchLogs(e events.KinesisEvent) ([]CloudWatchLogsData, error) {
	out := make([]CloudWatchLogsData, 0, len(e.Records))
	for i := range e.Records {
		data, err := ExtractCloudWatchLogsFromRecord(e.Records[i])
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		out = append(out, data)
	}
	return out, nil
}

// ExtractCloudWatchLogsFromRecord decodes the CloudWatch Logs subscription
// payload of one Kinesis record: its data is gzip-compressed JSON (the
// base64 layer is already gone, aws-lambda-go decodes it).
func ExtractCloudWatchLogsFromRecord(record events.KinesisEventRecord) (CloudWatchLogsData, error) {
	return decodeCloudWatchLogs(record.Kinesis.Data)
}

// KinesisTimeWindowEvent is events.KinesisTimeWindowEvent whose state can
// hold any JSON value, as the state a function returns can: aws-lambda-go
// only accepts strings, so an event carrying numbers (the AWS documentation
// example) fails to decode.
type KinesisTimeWindowEvent struct {
	events.KinesisTimeWindowEvent

	State map[string]any `json:"state"`
}

// KinesisTimeWindowEventResponse is events.KinesisTimeWindowEventResponse
// whose state can hold any JSON value.
type KinesisTimeWindowEventResponse struct {
	events.KinesisTimeWindowEventResponse

	State map[string]any `json:"state"`
}

// DynamoDBTimeWindowEvent is events.DynamoDBTimeWindowEvent whose state can
// hold any JSON value.
type DynamoDBTimeWindowEvent struct {
	events.DynamoDBTimeWindowEvent

	State map[string]any `json:"state"`
}

// DynamoDBTimeWindowEventResponse is events.DynamoDBTimeWindowEventResponse
// whose state can hold any JSON value.
type DynamoDBTimeWindowEventResponse struct {
	events.DynamoDBTimeWindowEventResponse

	State map[string]any `json:"state"`
}
