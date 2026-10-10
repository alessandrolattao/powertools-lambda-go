package batch_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/batch"
)

type order struct {
	ID    string  `json:"id"`
	Total float64 `json:"total"`
}

var errNoID = errors.New("an order needs an id")

func (o order) Validate() error {
	if o.ID == "" {
		return errNoID
	}
	return nil
}

func TestSQSJSON(t *testing.T) {
	t.Parallel()
	var got []order
	handler := batch.SQSJSON(func(_ context.Context, o order) error {
		got = append(got, o)
		return nil
	})
	var failures []error
	resp, err := batch.ProcessSQS(t.Context(), sqsEvent(
		`{"id":"o-1","total":12.5,"unknown":true}`,
		`not json`,
		`{"total":3}`,
		`{"id":"o-2","total":"wrong type"}`,
	), handler, batch.WithOnFailure(func(_ context.Context, _ events.SQSMessage, err error) {
		failures = append(failures, err)
	}))
	require.NoError(t, err)
	assert.Equal(t, []order{{ID: "o-1", Total: 12.5}}, got)
	assert.Equal(t, []string{"m1", "m2", "m3"}, ids(resp))
	require.Len(t, failures, 3)
	for _, err := range failures {
		require.ErrorIs(t, err, batch.ErrDecode)
	}
	require.ErrorIs(t, failures[1], errNoID, "Validate runs after decoding")
}

func TestKinesisJSON(t *testing.T) {
	t.Parallel()
	var got order
	event := events.KinesisEvent{Records: []events.KinesisEventRecord{
		{Kinesis: events.KinesisRecord{SequenceNumber: "1", Data: []byte(`{"id":"o-7","total":1}`)}},
		{Kinesis: events.KinesisRecord{SequenceNumber: "2", Data: []byte(`{`)}},
	}}
	resp, err := batch.ProcessKinesis(t.Context(), event, batch.KinesisJSON(func(_ context.Context, o order) error {
		got = o
		return nil
	}))
	require.NoError(t, err)
	assert.Equal(t, order{ID: "o-7", Total: 1}, got)
	assert.Equal(t, []events.KinesisBatchItemFailure{{ItemIdentifier: "2"}}, resp.BatchItemFailures)
}

func TestKafkaJSON(t *testing.T) {
	t.Parallel()
	encoded := base64.StdEncoding.EncodeToString([]byte(`{"id":"o-9","total":2}`))
	var got order
	event := events.KafkaEvent{Records: map[string][]events.KafkaRecord{"orders-0": {
		{Topic: "orders", Offset: 1, Value: encoded},
		{Topic: "orders", Offset: 2, Value: "%%% not base64"},
	}}}
	var failure error
	resp, err := batch.ProcessKafka(t.Context(), event, batch.KafkaJSON(func(_ context.Context, o order) error {
		got = o
		return nil
	}), batch.WithOnFailure(func(_ context.Context, _ events.KafkaRecord, err error) { failure = err }))
	require.NoError(t, err)
	assert.Equal(t, order{ID: "o-9", Total: 2}, got)
	require.Len(t, resp.BatchItemFailures, 1)
	assert.Equal(t, int64(2), resp.BatchItemFailures[0].ItemIdentifier.Offset)
	require.ErrorIs(t, failure, batch.ErrDecode)
}

type item struct {
	PK      string            `json:"pk"`
	Count   int64             `json:"count"`
	Price   float64           `json:"price"`
	Active  bool              `json:"active"`
	Note    *string           `json:"note"`
	Blob    []byte            `json:"blob"`
	Tags    []string          `json:"tags"`
	Sizes   []int             `json:"sizes"`
	Chunks  [][]byte          `json:"chunks"`
	Address map[string]string `json:"address"`
	History []any             `json:"history"`
}

func TestDynamoDBImages(t *testing.T) {
	t.Parallel()
	image := map[string]events.DynamoDBAttributeValue{
		"pk":      events.NewStringAttribute("p-1"),
		"count":   events.NewNumberAttribute("12345678901234567"),
		"price":   events.NewNumberAttribute("9.99"),
		"active":  events.NewBooleanAttribute(true),
		"note":    events.NewNullAttribute(),
		"blob":    events.NewBinaryAttribute([]byte{1, 2, 3}),
		"tags":    events.NewStringSetAttribute([]string{"a", "b"}),
		"sizes":   events.NewNumberSetAttribute([]string{"1", "2"}),
		"chunks":  events.NewBinarySetAttribute([][]byte{{9}}),
		"address": events.NewMapAttribute(map[string]events.DynamoDBAttributeValue{"city": events.NewStringAttribute("Rome")}),
		"history": events.NewListAttribute([]events.DynamoDBAttributeValue{
			events.NewStringAttribute("x"), events.NewNumberAttribute("1"),
		}),
	}
	want := item{
		PK: "p-1", Count: 12345678901234567, Price: 9.99, Active: true,
		Blob: []byte{1, 2, 3}, Tags: []string{"a", "b"}, Sizes: []int{1, 2}, Chunks: [][]byte{{9}},
		Address: map[string]string{"city": "Rome"}, History: []any{"x", 1.0},
	}

	cases := []struct {
		name     string
		change   events.DynamoDBStreamRecord
		wantNew  *item
		wantOld  *item
		wantFail bool
	}{
		{name: "insert", change: events.DynamoDBStreamRecord{NewImage: image}, wantNew: &want},
		{name: "remove", change: events.DynamoDBStreamRecord{OldImage: image}, wantOld: &want},
		{name: "modify", change: events.DynamoDBStreamRecord{NewImage: image, OldImage: image}, wantNew: &want, wantOld: &want},
		{name: "keys only", change: events.DynamoDBStreamRecord{}},
		{
			name: "bad number",
			change: events.DynamoDBStreamRecord{NewImage: map[string]events.DynamoDBAttributeValue{
				"count": events.NewNumberAttribute("twelve"),
			}},
			wantFail: true,
		},
		{
			name: "bad old image",
			change: events.DynamoDBStreamRecord{OldImage: map[string]events.DynamoDBAttributeValue{
				"count": events.NewStringAttribute("twelve"),
			}},
			wantFail: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotNew, gotOld *item
			ran := false
			handler := batch.DynamoDBImages(func(_ context.Context, newImage, oldImage *item) error {
				ran = true
				gotNew, gotOld = newImage, oldImage
				return nil
			})
			var failure error
			_, err := batch.ProcessDynamoDB(t.Context(),
				events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{{Change: tc.change}}},
				handler,
				batch.WithRaiseOnEntireBatchFailure(false),
				batch.WithOnFailure(func(_ context.Context, _ events.DynamoDBEventRecord, err error) { failure = err }))
			require.NoError(t, err)
			if tc.wantFail {
				require.ErrorIs(t, failure, batch.ErrDecode)
				assert.False(t, ran)
				return
			}
			require.NoError(t, failure)
			assert.Equal(t, tc.wantNew, gotNew)
			assert.Equal(t, tc.wantOld, gotOld)
		})
	}
}
