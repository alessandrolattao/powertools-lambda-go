package kafka_test

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/kafka"
)

// loadEvent reads one of the sample events: the events of Powertools
// Python's tests and examples, recorded from real event source mappings.
func loadEvent(t *testing.T, name string) *kafka.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var event kafka.Event
	require.NoError(t, json.Unmarshal(data, &event))
	return &event
}

func TestEventMSK(t *testing.T) {
	t.Parallel()
	event := loadEvent(t, "msk.json")

	assert.Equal(t, kafka.EventSourceMSK, event.EventSource)
	assert.Equal(t, "arn:aws:kafka:us-east-1:0123456789019:cluster/SalesCluster/abcd1234-abcd-cafe-abab-9876543210ab-4", event.EventSourceARN)
	assert.Equal(t, []string{
		"b-2.demo-cluster-1.a1bcde.c1.kafka.us-east-1.amazonaws.com:9092",
		"b-1.demo-cluster-1.a1bcde.c1.kafka.us-east-1.amazonaws.com:9092",
	}, event.BootstrapServerList())
	require.Equal(t, 4, event.Len())

	records := slices.Collect(event.All())
	first := records[0]
	assert.Equal(t, "mytopic", first.Topic)
	assert.Equal(t, int64(0), first.Partition)
	assert.Equal(t, int64(15), first.Offset)
	assert.Equal(t, time.UnixMilli(1545084650987), first.Time())
	assert.Equal(t, kafka.TimestampCreateTime, first.TimestampType)
	assert.Equal(t, "mytopic-0", first.TopicPartition())
	require.NotNil(t, first.Key)
	assert.Equal(t, "cmVjb3JkS2V5", *first.Key)
	require.NotNil(t, first.Value)
	assert.Equal(t, "eyJrZXkiOiJ2YWx1ZSJ9", *first.Value)
	assert.Equal(t, kafka.Headers{{Key: "headerKey", Value: []byte("headerValue")}}, first.Headers)
	assert.Equal(t, &kafka.SchemaMetadata{DataFormat: kafka.FormatAvro, SchemaID: "1234"}, first.ValueSchemaMetadata)
	assert.Equal(t, &kafka.SchemaMetadata{DataFormat: kafka.FormatAvro, SchemaID: "1234"}, first.KeySchemaMetadata)

	assert.Nil(t, records[1].Key, "absent key")
	assert.Nil(t, records[2].Key, "null key")

	value, ok := records[3].Headers.Get("headerkey")
	require.True(t, ok)
	assert.Equal(t, "hello-world-ë", string(value), "signed bytes, as Lambda writes them")
}

func TestEventSelfManaged(t *testing.T) {
	t.Parallel()
	event := loadEvent(t, "self_managed.json")

	assert.Equal(t, kafka.EventSourceSelfManaged, event.EventSource)
	assert.Empty(t, event.EventSourceARN)
	assert.Equal(t, 3, event.Len())
	for record := range event.All() {
		assert.Nil(t, record.ValueSchemaMetadata)
		assert.Nil(t, record.KeySchemaMetadata)
	}
}

func TestEventKeepsTheOrderOfPartitions(t *testing.T) {
	t.Parallel()
	payload := `{"eventSource":"aws:kafka","records":{
		"orders-2":[{"topic":"orders","partition":2,"offset":7,"value":"Mg=="},{"topic":"orders","partition":2,"offset":8,"value":"Mw=="}],
		"orders-0":[{"topic":"orders","partition":0,"offset":1,"value":"MA=="}],
		"audit-1":[{"topic":"audit","partition":1,"offset":3,"value":"MQ=="}]
	}}`
	var event kafka.Event
	require.NoError(t, json.Unmarshal([]byte(payload), &event))

	names := make([]string, 0, len(event.Records))
	for _, tp := range event.Records {
		names = append(names, tp.Name)
	}
	assert.Equal(t, []string{"orders-2", "orders-0", "audit-1"}, names)

	var offsets []int64
	for record := range event.All() {
		offsets = append(offsets, record.Offset)
	}
	assert.Equal(t, []int64{7, 8, 1, 3}, offsets)

	var stopped []int64
	for record := range event.All() {
		stopped = append(stopped, record.Offset)
		if len(stopped) == 2 {
			break
		}
	}
	assert.Equal(t, []int64{7, 8}, stopped)
}

func TestEventRoundTrip(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"msk.json", "self_managed.json", "protobuf_confluent.json", "protobuf_glue.json"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			event := loadEvent(t, name)
			encoded, err := json.Marshal(event)
			require.NoError(t, err)

			var again kafka.Event
			require.NoError(t, json.Unmarshal(encoded, &again))
			assert.Equal(t, event, &again)
		})
	}
}

func TestEventMarshalsHeadersSigned(t *testing.T) {
	t.Parallel()
	event := kafka.Event{
		EventSource: kafka.EventSourceSelfManaged,
		Records: kafka.TopicPartitions{{Name: "t-0", Records: []kafka.Record{{
			Topic:   "t",
			Value:   new("eA=="),
			Headers: kafka.Headers{{Key: "k", Value: []byte{0x68, 0xc3, 0xab}}},
		}}}},
	}
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"eventSource":"SelfManagedKafka","bootstrapServers":"",
		"records":{"t-0":[{"topic":"t","partition":0,"offset":0,"timestamp":0,"timestampType":"",
			"value":"eA==","headers":[{"k":[104,-61,-85]}]}]}
	}`, string(encoded))
}

func TestEventNullAndEmpty(t *testing.T) {
	t.Parallel()
	var event kafka.Event
	require.NoError(t, json.Unmarshal([]byte(`{"records":null}`), &event))
	assert.Zero(t, event.Len())
	assert.Nil(t, event.BootstrapServerList())

	var nilEvent *kafka.Event
	assert.Zero(t, nilEvent.Len())
	assert.Empty(t, slices.Collect(nilEvent.All()))
	assert.Nil(t, nilEvent.BootstrapServerList())

	encoded, err := json.Marshal(kafka.Event{Records: kafka.TopicPartitions{{Name: "t-0"}}})
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"records":{"t-0":[]}`)
}

func TestEventRejectsMalformedRecords(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"records not an object":    `{"records":[]}`,
		"partition not an array":   `{"records":{"t-0":{}}}`,
		"headers not an array":     `{"records":{"t-0":[{"headers":{}}]}}`,
		"header not an object":     `{"records":{"t-0":[{"headers":[[1]]}]}}`,
		"header value not array":   `{"records":{"t-0":[{"headers":[{"k":"v"}]}]}}`,
		"header byte not a number": `{"records":{"t-0":[{"headers":[{"k":["a"]}]}]}}`,
		"header byte too large":    `{"records":{"t-0":[{"headers":[{"k":[256]}]}]}}`,
		"header byte too small":    `{"records":{"t-0":[{"headers":[{"k":[-129]}]}]}}`,
		"header byte fractional":   `{"records":{"t-0":[{"headers":[{"k":[1.5]}]}]}}`,
		"truncated records":        `{"records":{"t-0":[`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var event kafka.Event
			require.Error(t, json.Unmarshal([]byte(payload), &event))
		})
	}
}

func TestHeaders(t *testing.T) {
	t.Parallel()
	var headers kafka.Headers
	require.NoError(t, json.Unmarshal([]byte(`[
		{"Trace-Id":[49]},
		{"retry":[255,0]},
		{"trace-id":[50],"other":[]},
		{"empty":null}
	]`), &headers))

	assert.Equal(t, kafka.Headers{
		{Key: "Trace-Id", Value: []byte("1")},
		{Key: "retry", Value: []byte{0xff, 0x00}},
		{Key: "trace-id", Value: []byte("2")},
		{Key: "other", Value: []byte{}},
		{Key: "empty"},
	}, headers)

	value, ok := headers.Get("TRACE-ID")
	require.True(t, ok)
	assert.Equal(t, []byte("2"), value, "the last header of a repeated key wins, as in Python")
	assert.Equal(t, [][]byte{[]byte("1"), []byte("2")}, headers.Values("trace-id"))
	_, ok = headers.Get("missing")
	assert.False(t, ok)
	assert.Nil(t, headers.Values("missing"))

	var keys []string
	for key := range headers.All() {
		keys = append(keys, key)
		if key == "trace-id" {
			break
		}
	}
	assert.Equal(t, []string{"Trace-Id", "retry", "trace-id"}, keys)

	require.NoError(t, json.Unmarshal([]byte(`null`), &headers))
	assert.Nil(t, headers)
}

func TestFromLambdaEvent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "msk.json"))
	require.NoError(t, err)
	var lambdaEvent events.KafkaEvent
	require.NoError(t, json.Unmarshal(data, &lambdaEvent))

	event := kafka.FromLambdaEvent(lambdaEvent)
	assert.Equal(t, kafka.EventSourceMSK, event.EventSource)
	assert.Equal(t, lambdaEvent.EventSourceARN, event.EventSourceARN)
	require.Equal(t, 4, event.Len())

	first := event.Records[0].Records[0]
	assert.Equal(t, int64(1545084650987), first.Timestamp)
	assert.Equal(t, "cmVjb3JkS2V5", *first.Key)
	assert.Equal(t, "eyJrZXkiOiJ2YWx1ZSJ9", *first.Value)
	assert.Equal(t, kafka.Headers{{Key: "headerKey", Value: []byte("headerValue")}}, first.Headers)
	assert.Nil(t, first.ValueSchemaMetadata, "aws-lambda-go drops the schema metadata")
	assert.Nil(t, event.Records[0].Records[1].Key)

	tombstone := kafka.FromLambdaRecord(events.KafkaRecord{Topic: "t"})
	assert.Nil(t, tombstone.Value)
	assert.Nil(t, tombstone.Key)
}

func TestSchemaMetadataRegistry(t *testing.T) {
	t.Parallel()
	assert.Equal(t, kafka.RegistryConfluent, (&kafka.SchemaMetadata{SchemaID: "1234"}).Registry())
	assert.Equal(t, kafka.RegistryGlue, (&kafka.SchemaMetadata{SchemaID: "7d55d475-2244-4485-8341-f74468c1e058"}).Registry())
}
