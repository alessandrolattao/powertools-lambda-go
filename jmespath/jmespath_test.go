package jmespath

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gzipBase64(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestEnvelopes(t *testing.T) {
	t.Parallel()
	order := `{"id":"o-1","total":12.5}`
	want := map[string]any{"id": "o-1", "total": 12.5}

	cases := []struct {
		name     string
		envelope string
		event    any
		want     any
	}{
		{
			name:     "API Gateway REST",
			envelope: APIGatewayREST,
			event:    events.APIGatewayProxyRequest{Body: order},
			want:     want,
		},
		{
			name:     "API Gateway HTTP",
			envelope: APIGatewayHTTP,
			event:    events.APIGatewayV2HTTPRequest{Body: order},
			want:     want,
		},
		{
			name:     "SQS",
			envelope: SQS,
			event:    events.SQSEvent{Records: []events.SQSMessage{{Body: order}, {Body: order}}},
			want:     []any{want, want},
		},
		{
			name:     "SNS",
			envelope: SNS,
			event:    events.SNSEvent{Records: []events.SNSEventRecord{{SNS: events.SNSEntity{Message: order}}}},
			want:     want,
		},
		{
			name:     "EventBridge",
			envelope: EventBridge,
			event:    events.EventBridgeEvent{Detail: jsontext.Value(order)},
			want:     want,
		},
		{
			name:     "Kinesis",
			envelope: KinesisDataStream,
			event: events.KinesisEvent{Records: []events.KinesisEventRecord{
				{Kinesis: events.KinesisRecord{Data: []byte(order)}},
			}},
			want: []any{want},
		},
		{
			name:     "CloudWatch Logs",
			envelope: CloudWatchLogs,
			event: map[string]any{"awslogs": map[string]any{
				"data": gzipBase64(t, `{"logEvents":[{"id":"1","message":"hello"}]}`),
			}},
			want: []any{map[string]any{"id": "1", "message": "hello"}},
		},
		{
			name:     "S3 through SQS",
			envelope: S3SQS,
			event: events.SQSEvent{Records: []events.SQSMessage{
				{Body: `{"Records":[{"s3":{"object":{"key":"a.txt"}}}]}`},
			}},
			want: []any{map[string]any{"s3": map[string]any{"object": map[string]any{"key": "a.txt"}}}},
		},
		{
			name:     "S3 through SNS and SQS",
			envelope: S3SNSSQS,
			event: events.SQSEvent{Records: []events.SQSMessage{
				{Body: `{"Message":"{\"Records\":[{\"eventName\":\"ObjectCreated:Put\"}]}"}`},
			}},
			want: []any{map[string]any{"eventName": "ObjectCreated:Put"}},
		},
		{
			name:     "S3 through Kinesis Firehose",
			envelope: S3KinesisFirehose,
			event: map[string]any{"records": []any{
				map[string]any{"data": b64(`{"Records":[{"eventName":"ObjectRemoved:Delete"}]}`)},
			}},
			want: []any{map[string]any{"eventName": "ObjectRemoved:Delete"}},
		},
		{
			name:     "S3 through SNS and Kinesis Firehose",
			envelope: S3SNSKinesisFirehose,
			event: map[string]any{"records": []any{
				map[string]any{"data": b64(`{"Message":"{\"Records\":[{\"eventName\":\"ObjectCreated:Copy\"}]}"}`)},
			}},
			want: []any{map[string]any{"eventName": "ObjectCreated:Copy"}},
		},
		{
			name:     "S3 through EventBridge and SQS",
			envelope: S3EventBridgeSQS,
			event: events.SQSEvent{Records: []events.SQSMessage{
				{Body: `{"detail":{"bucket":{"name":"b"}}}`},
			}},
			want: []any{map[string]any{"bucket": map[string]any{"name": "b"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Query(tc.event, tc.envelope)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestQueryRawJSON(t *testing.T) {
	t.Parallel()
	for _, data := range []any{[]byte(`{"a":{"b":[1,2]}}`), jsontext.Value(`{"a":{"b":[1,2]}}`)} {
		got, err := Query(data, "a.b[1]")
		require.NoError(t, err)
		assert.InDelta(t, 2.0, got, 0)
	}
}

func TestQueryNoMatch(t *testing.T) {
	t.Parallel()
	got, err := Query(map[string]any{"a": 1}, "missing.key")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestQueryAs(t *testing.T) {
	t.Parallel()
	type order struct {
		ID    string  `json:"id"`
		Total float64 `json:"total"`
	}
	got, err := QueryAs[[]order](events.SQSEvent{Records: []events.SQSMessage{
		{Body: `{"id":"a","total":1}`}, {Body: `{"id":"b","total":2}`},
	}}, SQS)
	require.NoError(t, err)
	assert.Equal(t, []order{{ID: "a", Total: 1}, {ID: "b", Total: 2}}, got)

	_, err = QueryAs[int](map[string]any{"a": "text"}, "a")
	require.ErrorIs(t, err, ErrInvalidEnvelope)
}

func TestErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		expression string
		data       any
		cause      error
	}{
		{name: "syntax", expression: "Records[*", data: map[string]any{}},
		{name: "unknown function", expression: "nope(a)", data: map[string]any{"a": "x"}},
		{name: "invalid JSON string", expression: "powertools_json(a)", data: map[string]any{"a": "{not json"}},
		{name: "wrong argument type", expression: "powertools_json(a)", data: map[string]any{"a": 3}},
		{name: "invalid base64", expression: "powertools_base64(a)", data: map[string]any{"a": "%%%"}},
		{
			name: "base64 that is not text", expression: "powertools_base64(a)",
			data:  map[string]any{"a": base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe})},
			cause: ErrNotUTF8,
		},
		{name: "base64 that is not gzip", expression: "powertools_base64_gzip(a)", data: map[string]any{"a": b64("plain")}},
		{name: "invalid raw JSON data", expression: "a", data: []byte("{")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Query(tc.data, tc.expression)
			require.ErrorIs(t, err, ErrInvalidEnvelope)
			envelopeErr, ok := errors.AsType[*EnvelopeError](err)
			require.True(t, ok)
			assert.Equal(t, tc.expression, envelopeErr.Expression)
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
			}
		})
	}
}

func TestInflateLimit(t *testing.T) {
	t.Parallel()
	compressed, err := base64.StdEncoding.DecodeString(gzipBase64(t, strings.Repeat("a", 1025)))
	require.NoError(t, err)

	_, err = inflate(compressed, 1024)
	require.ErrorIs(t, err, ErrTooLarge)

	decoded, err := inflate(compressed, 1025)
	require.NoError(t, err)
	assert.Len(t, decoded, 1025)
}

func TestCustomFunction(t *testing.T) {
	t.Parallel()
	upper := Function{
		Name: "upper",
		Args: []Arg{{Types: []Type{TypeString}}},
		Call: func(args []any) (any, error) {
			s, ok := args[0].(string)
			if !ok {
				return nil, errors.New("not a string")
			}
			return strings.ToUpper(s), nil
		},
	}
	got, err := Query(map[string]any{"body": `{"name":"ada"}`}, "upper(powertools_json(body).name)", WithFunctions(upper))
	require.NoError(t, err)
	assert.Equal(t, "ADA", got)
}

func TestExpressionIsReusableConcurrently(t *testing.T) {
	t.Parallel()
	e := MustCompile(SQS)
	assert.Equal(t, SQS, e.String())
	results := make([]any, 16)
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = e.Search(events.SQSEvent{Records: []events.SQSMessage{{Body: `{"n":1}`}}})
		})
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		assert.Equal(t, []any{map[string]any{"n": 1.0}}, results[i])
	}
}

func TestMustCompilePanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { MustCompile("a[") })
}

func TestGeneric(t *testing.T) {
	t.Parallel()
	for _, v := range []any{nil, "s", true, 1.5} {
		got, err := Generic(v)
		require.NoError(t, err)
		assert.Equal(t, v, got)
	}
	_, err := Generic(make(chan int))
	require.Error(t, err)
}
