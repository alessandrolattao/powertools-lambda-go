package batch

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

type fuzzTarget struct {
	ID    string         `json:"id"`
	Count int            `json:"count"`
	Tags  []string       `json:"tags"`
	Extra map[string]any `json:"extra"`
}

// FuzzDecodeJSON feeds arbitrary payloads to the typed adapters: whatever a
// record carries, they decode it or fail the record with ErrDecode, never
// panic.
func FuzzDecodeJSON(f *testing.F) {
	f.Add([]byte(`{"id":"a","count":1,"tags":["x"]}`))
	f.Add([]byte(`{"id":1}`))
	f.Add([]byte(`[`))
	f.Fuzz(func(t *testing.T, data []byte) {
		accept := func(context.Context, fuzzTarget) error { return nil }
		ctx := t.Context()
		for _, err := range []error{
			SQSJSON(accept)(ctx, events.SQSMessage{Body: string(data)}),
			KinesisJSON(accept)(ctx, events.KinesisEventRecord{Kinesis: events.KinesisRecord{Data: data}}),
			KafkaJSON(accept)(ctx, events.KafkaRecord{Value: string(data)}),
			KafkaJSON(accept)(ctx, events.KafkaRecord{Value: base64.StdEncoding.EncodeToString(data)}),
		} {
			if err != nil && !errors.Is(err, ErrDecode) {
				t.Fatalf("decoding %q failed without ErrDecode: %v", data, err)
			}
		}
	})
}

// FuzzImageJSON feeds arbitrary DynamoDB images to the conversion to JSON:
// it fails or produces valid JSON.
func FuzzImageJSON(f *testing.F) {
	f.Add(`{"a":{"S":"x"},"b":{"N":"1.5e3"},"c":{"L":[{"BOOL":true},{"NULL":true}]}}`)
	f.Add(`{"a":{"NS":["1","x"]},"b":{"BS":["AQI="]},"c":{"M":{"d":{"SS":["y"]}}}}`)
	f.Add(`{"a":{"N":"\"1\""}}`)
	f.Fuzz(func(t *testing.T, data string) {
		var image map[string]events.DynamoDBAttributeValue
		if err := json.Unmarshal([]byte(data), &image); err != nil {
			return
		}
		out, err := imageJSON(image)
		if err != nil {
			return
		}
		if !jsontext.Value(out).IsValid() {
			t.Fatalf("invalid JSON %q from %q", out, data)
		}
	})
}

// FuzzCompareTopicPartition checks that the order of Kafka partitions is a
// total order whatever the keys look like.
func FuzzCompareTopicPartition(f *testing.F) {
	f.Add("orders-1", "orders-10")
	f.Add("a-b-1", "a-b")
	f.Add("-1", "x-")
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := compareTopicPartition(a, b), compareTopicPartition(b, a)
		if ab != -ba {
			t.Fatalf("compare(%q, %q) = %d but compare(%q, %q) = %d", a, b, ab, b, a, ba)
		}
		if (ab == 0) != (a == b) {
			t.Fatalf("compare(%q, %q) = %d", a, b, ab)
		}
	})
}
