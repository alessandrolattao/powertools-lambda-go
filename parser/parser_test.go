package parser

// The JSON samples in testdata are the sample events of Powertools for AWS
// Lambda (Python), tests/events, MIT No Attribution.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

func sample(t *testing.T, name string) jsontext.Value {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

func decodeSample[T any](t *testing.T, name string) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(sample(t, name), &out))
	return out
}

var errInvalid = errors.New("invalid")

// message is a payload model with a Validate method.
type message struct {
	Message  string `json:"message"`
	Username string `json:"username"`
}

func (m message) Validate() error {
	if m.Message == "" {
		return errInvalid
	}
	return nil
}

// keyValue is the payload of the Kafka samples.
type keyValue struct {
	Key string `json:"key"`
}

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data any
	}{
		{"bytes", []byte(`{"message":"hi","username":"tom"}`)},
		{"raw", jsontext.Value(`{"message":"hi","username":"tom"}`)},
		{"string", `{"message":"hi","username":"tom"}`},
		{"map", map[string]any{"message": "hi", "username": "tom"}},
		{"struct", message{Message: "hi", Username: "tom"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse[message](tc.data)
			require.NoError(t, err)
			assert.Equal(t, message{Message: "hi", Username: "tom"}, got)
		})
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	_, err := Parse[message](`{"message":""}`)
	require.ErrorIs(t, err, ErrParse)
	require.ErrorIs(t, err, errInvalid)
	parseErr, ok := errors.AsType[*ParseError](err)
	require.True(t, ok)
	assert.Equal(t, "parser.message", parseErr.Model)
	assert.Empty(t, parseErr.Source)
	assert.Contains(t, err.Error(), "parse parser.message")

	_, err = Parse[message](`{"message":`)
	require.ErrorIs(t, err, ErrParse)
	syntactic, ok := errors.AsType[*jsontext.SyntacticError](err)
	require.True(t, ok)
	assert.NotNil(t, syntactic)

	_, err = Parse[message](`{"message":1}`)
	semantic, ok := errors.AsType[*json.SemanticError](err)
	require.True(t, ok)
	assert.Equal(t, "/message", string(semantic.JSONPointer))

	_, err = Parse[message](make(chan int))
	require.ErrorIs(t, err, ErrParse)

	_, err = Parse[message](`{"message":"hi","extra":1}`, WithJSONOptions(json.RejectUnknownMembers(true)))
	require.ErrorIs(t, err, ErrParse)

	_, err = Parse[[]message](`{"Records":[]}`, WithEnvelope(SQSEnvelope))
	require.NoError(t, err)
	_, err = Parse[[]message](`{"Records":"no"}`, WithEnvelope(SQSEnvelope))
	require.ErrorIs(t, err, ErrInvalidEnvelope)
}

// pointerValidated validates through a pointer receiver.
type pointerValidated struct {
	N int `json:"n"`
}

func (p *pointerValidated) Validate() error {
	if p.N < 0 {
		return errInvalid
	}
	return nil
}

func TestValidate(t *testing.T) {
	t.Parallel()

	_, err := Parse[pointerValidated](`{"n":-1}`)
	require.ErrorIs(t, err, errInvalid)

	_, err = Parse[[]message](`[{"message":"a"},{"message":""}]`)
	require.ErrorIs(t, err, errInvalid)
	assert.Contains(t, err.Error(), "element 1")

	_, err = Parse[[2]pointerValidated](`[{"n":1},{"n":-2}]`)
	require.ErrorIs(t, err, errInvalid)

	got, err := Parse[[]*message](`[{"message":"a"},null]`)
	require.NoError(t, err, "a nil element is a missing payload")
	assert.Nil(t, got[1])

	null, err := Parse[*message](`null`)
	require.NoError(t, err)
	assert.Nil(t, null)

	none, err := Parse[message](``)
	require.NoError(t, err)
	assert.Zero(t, none)

	plain, err := Parse[map[string]int](`{"a":1}`)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 1}, plain)
}

func TestJSON(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		Body JSON[message] `json:"body"`
	}
	got, err := Parse[wrapper](`{"body":"{\"message\":\"hi\"}"}`)
	require.NoError(t, err)
	assert.Equal(t, "hi", got.Body.Value.Message)

	direct, err := Parse[wrapper](`{"body":{"message":"hi"}}`)
	require.NoError(t, err)
	assert.Equal(t, got, direct)

	out, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"body":"{\"message\":\"hi\",\"username\":\"\"}"}`, string(out))

	require.NoError(t, got.Body.Validate())
	require.ErrorIs(t, wrapper{}.Body.Validate(), errInvalid)

	_, err = Parse[wrapper](`{"body":"not json"}`)
	require.ErrorIs(t, err, ErrParse)
	_, err = Parse[wrapper](`{"body":"{\"message\":1}"}`)
	require.ErrorIs(t, err, ErrParse)

	s3 := decodeSample[events.SQSEvent](t, "s3SqsEvent.json")
	type s3Message struct {
		events.SQSMessage

		Body JSON[events.S3Event] `json:"body"`
	}
	type s3Messages struct {
		Records []s3Message `json:"Records"`
	}
	nested, err := Parse[s3Messages](s3)
	require.NoError(t, err)
	assert.Equal(t, "ObjectCreated:Put", nested.Records[0].Body.Value.Records[0].EventName)
	assert.NotEmpty(t, nested.Records[0].MessageId)
}

// envelopeCase runs a typed helper and the matching Envelope on the same
// event, and checks both give want.
type envelopeCase struct {
	name     string
	envelope Envelope
	raw      jsontext.Value
	typed    func() (any, error)
	parse    func(raw jsontext.Value, env Envelope) (any, error)
	want     any
}

func single[T any](raw jsontext.Value, env Envelope) (any, error) {
	return Parse[T](raw, WithEnvelope(env))
}

func marshal(t *testing.T, v any) jsontext.Value {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestEnvelopes(t *testing.T) {
	t.Parallel()
	hello := message{Message: "hello world", Username: "tom"}
	lessa := message{Message: "hello world", Username: "lessa"}
	payload := `{"message":"hello world","username":"tom"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))

	httpAPI := decodeSample[events.APIGatewayV2HTTPRequest](t, "apiGatewayProxyV2Event.json")
	ws := decodeSample[events.APIGatewayWebsocketProxyRequest](t, "apiGatewayWebSocketApiMessage.json")
	snsSQSEvent := decodeSample[events.SQSEvent](t, "snsSqsEvent.json")
	fifo := decodeSample[events.SQSEvent](t, "snsSqsFifoEvent.json")
	eventBridge := decodeSample[events.EventBridgeEvent](t, "eventBridgeEvent.json")
	scheduler := decodeSample[events.EventBridgeEvent](t, "eventBridgeSchedulerEvent.json")
	latticeV1 := decodeSample[eventsource.VPCLatticeEvent](t, "vpcLatticeEvent.json")
	latticeV2 := decodeSample[eventsource.VPCLatticeEventV2](t, "vpcLatticeV2Event.json")
	kafka := decodeSample[events.KafkaEvent](t, "kafkaEventMsk.json")
	rest := events.APIGatewayProxyRequest{Body: b64, IsBase64Encoded: true}
	url := events.LambdaFunctionURLRequest{Body: payload}
	sqsEvent := events.SQSEvent{Records: []events.SQSMessage{{Body: payload}, {Body: payload}}}
	snsEvent := events.SNSEvent{Records: []events.SNSEventRecord{{SNS: events.SNSEntity{Message: payload}}}}
	kinesisEvent := events.KinesisEvent{Records: []events.KinesisEventRecord{
		{Kinesis: events.KinesisRecord{Data: []byte(payload)}},
		{Kinesis: events.KinesisRecord{Data: gzipped(t, payload)}},
	}}
	firehoseEvent := events.KinesisFirehoseEvent{Records: []events.KinesisFirehoseEventRecord{{Data: []byte(payload)}}}
	logs := events.CloudwatchLogsEvent{AWSLogs: events.CloudwatchLogsRawData{Data: base64.StdEncoding.EncodeToString(gzipped(t,
		`{"logEvents":[{"id":"1","timestamp":1,"message":"{\"message\":\"hello world\",\"username\":\"tom\"}"}]}`))}}
	agent := eventsource.BedrockAgentEvent{InputText: payload}
	agentFunction := eventsource.BedrockAgentFunctionEvent{InputText: payload}

	cases := []envelopeCase{
		{
			"API Gateway REST base64", APIGatewayRESTEnvelope, marshal(t, rest),
			func() (any, error) { return APIGatewayREST[message](rest) }, single[message], hello,
		},
		{
			"API Gateway HTTP", APIGatewayHTTPEnvelope, sample(t, "apiGatewayProxyV2Event.json"),
			func() (any, error) { return APIGatewayHTTP[message](httpAPI) }, single[message], hello,
		},
		{
			"API Gateway WebSocket", APIGatewayWebSocketEnvelope, sample(t, "apiGatewayWebSocketApiMessage.json"),
			func() (any, error) { return APIGatewayWebSocket[message](ws) }, single[message],
			message{Message: "Hello from client"},
		},
		{
			"Function URL", FunctionURLEnvelope, marshal(t, url),
			func() (any, error) { return FunctionURL[message](url) }, single[message], hello,
		},
		{
			"VPC Lattice", VPCLatticeEnvelope, sample(t, "vpcLatticeEvent.json"),
			func() (any, error) { return VPCLattice[map[string]string](latticeV1) }, single[map[string]string],
			map[string]string{"test": "event"},
		},
		{
			"VPC Lattice v2", VPCLatticeV2Envelope, sample(t, "vpcLatticeV2Event.json"),
			func() (any, error) { return VPCLatticeV2[message](latticeV2) }, single[message],
			message{Message: "Hello from Lambda!"},
		},
		{
			"EventBridge", EventBridgeEnvelope, sample(t, "eventBridgeEvent.json"),
			func() (any, error) { return EventBridge[map[string]string](eventBridge) }, single[map[string]string],
			map[string]string{"instance_id": "i-1234567890abcdef0", "state": "terminated"},
		},
		{
			"EventBridge Scheduler", EventBridgeEnvelope, sample(t, "eventBridgeSchedulerEvent.json"),
			func() (any, error) { return EventBridge[map[string]string](scheduler) }, single[map[string]string],
			map[string]string{},
		},
		{
			"SQS", SQSEnvelope, marshal(t, sqsEvent),
			func() (any, error) { return SQS[message](sqsEvent) }, single[[]message],
			[]message{hello, hello},
		},
		{
			"SNS", SNSEnvelope, marshal(t, snsEvent),
			func() (any, error) { return SNS[message](snsEvent) }, single[[]message],
			[]message{hello},
		},
		{
			"SNS in SQS", SNSSQSEnvelope, sample(t, "snsSqsEvent.json"),
			func() (any, error) { return SNSSQS[message](snsSQSEvent) }, single[[]message],
			[]message{lessa},
		},
		{
			"SNS in SQS FIFO", SNSSQSEnvelope, sample(t, "snsSqsFifoEvent.json"),
			func() (any, error) { return SNSSQS[message](fifo) }, single[[]message],
			[]message{lessa},
		},
		{
			"CloudWatch Logs", CloudWatchLogsEnvelope, marshal(t, logs),
			func() (any, error) { return CloudWatchLogs[message](logs) }, single[[]message],
			[]message{hello},
		},
		{
			"Kinesis", KinesisEnvelope, marshal(t, kinesisEvent),
			func() (any, error) { return Kinesis[message](kinesisEvent) }, single[[]message],
			[]message{hello, hello},
		},
		{
			"Firehose", FirehoseEnvelope, marshal(t, firehoseEvent),
			func() (any, error) { return Firehose[message](firehoseEvent) }, single[[]message],
			[]message{hello},
		},
		{
			"Kafka", KafkaEnvelope, sample(t, "kafkaEventMsk.json"),
			func() (any, error) { return Kafka[keyValue](kafka) }, single[[]keyValue],
			[]keyValue{{"value"}, {"value"}, {"value"}, {"value"}},
		},
		{
			"Bedrock agent", BedrockAgentEnvelope, marshal(t, agent),
			func() (any, error) { return BedrockAgent[message](agent) }, single[message], hello,
		},
		{
			"Bedrock agent function", BedrockAgentFunctionEnvelope, marshal(t, agentFunction),
			func() (any, error) { return BedrockAgentFunction[message](agentFunction) }, single[message], hello,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typed, err := tc.typed()
			require.NoError(t, err)
			assert.Equal(t, tc.want, typed)
			parsed, err := tc.parse(tc.raw, tc.envelope)
			require.NoError(t, err)
			assert.Equal(t, tc.want, parsed)
		})
	}
}

func TestEnvelopeMissingPayloads(t *testing.T) {
	t.Parallel()

	get, err := APIGatewayREST[*message](decodeSample[events.APIGatewayProxyRequest](t, "apiGatewayProxyEvent.json"),
		WithJSONOptions())
	require.Error(t, err, "the sample body is plain text")
	assert.Nil(t, get)

	none, err := FunctionURL[*message](decodeSample[events.LambdaFunctionURLRequest](t, "lambdaFunctionUrlEvent.json"))
	require.NoError(t, err)
	assert.Nil(t, none)
	raw, err := Parse[*message](sample(t, "lambdaFunctionUrlEvent.json"), WithEnvelope(FunctionURLEnvelope))
	require.NoError(t, err)
	assert.Nil(t, raw)

	withNull, err := Parse[[]*message](marshal(t, events.SQSEvent{Records: []events.SQSMessage{{Body: ""}}}), WithEnvelope(SQSEnvelope))
	require.NoError(t, err)
	assert.Equal(t, []*message{nil}, withNull)

	empty, err := EventBridge[*message](events.EventBridgeEvent{})
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestEnvelopeErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		run    func() error
		source string
	}{
		{"invalid base64 body", func() error {
			_, err := APIGatewayREST[message](events.APIGatewayProxyRequest{Body: "!!", IsBase64Encoded: true})
			return err
		}, ""},
		{"record failing validation", func() error {
			_, err := SQS[message](events.SQSEvent{Records: []events.SQSMessage{{Body: `{"message":"a"}`}, {Body: `{}`}}})
			return err
		}, "Records[1].body"},
		{"record not JSON", func() error {
			_, err := SNS[message](events.SNSEvent{Records: []events.SNSEventRecord{{SNS: events.SNSEntity{Message: "Hello from SNS!"}}}})
			return err
		}, "Records[0].Sns.Message"},
		{"SNS in SQS without notification", func() error {
			_, err := SNSSQS[message](events.SQSEvent{Records: []events.SQSMessage{{Body: "nope"}}})
			return err
		}, ""},
		{"Kinesis data neither text nor compressed", func() error {
			_, err := Kinesis[message](events.KinesisEvent{Records: []events.KinesisEventRecord{{Kinesis: events.KinesisRecord{Data: []byte{0xff, 0xfe}}}}})
			return err
		}, ""},
		{"Firehose data not text", func() error {
			_, err := Firehose[message](events.KinesisFirehoseEvent{Records: []events.KinesisFirehoseEventRecord{{Data: []byte{0xff}}}})
			return err
		}, ""},
		{"CloudWatch Logs data not compressed", func() error {
			_, err := CloudWatchLogs[message](events.CloudwatchLogsEvent{AWSLogs: events.CloudwatchLogsRawData{Data: "aGk="}})
			return err
		}, ""},
		{"Kafka value not base64", func() error {
			_, err := Kafka[message](events.KafkaEvent{Records: map[string][]events.KafkaRecord{"t-0": {{Value: "!!"}}}})
			return err
		}, ""},
		{"Bedrock input text not JSON", func() error {
			_, err := BedrockAgent[message](decodeSample[eventsource.BedrockAgentEvent](t, "bedrockAgentEvent.json"))
			return err
		}, "inputText"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run()
			require.ErrorIs(t, err, ErrParse)
			if tc.source != "" {
				parseErr, ok := errors.AsType[*ParseError](err)
				require.True(t, ok)
				assert.Equal(t, tc.source, parseErr.Source)
			}
		})
	}

	rawCases := map[string]struct {
		envelope Envelope
		raw      string
	}{
		"payload not JSON":   {SQSEnvelope, `{"Records":[{"body":"Test message."}]}`},
		"single not JSON":    {APIGatewayRESTEnvelope, `{"body":"Hello"}`},
		"Kafka records":      {KafkaEnvelope, `{"records":{"t-0":"nope"}}`},
		"Kafka value":        {KafkaEnvelope, `{"records":{"t-0":[{"value":"!!"}]}}`},
		"Kafka not an event": {KafkaEnvelope, `[]`},
		"DynamoDB number":    {DynamoDBStreamEnvelope, `{"Records":[{"dynamodb":{"NewImage":{"n":{"N":"1x"}}}}]}`},
		"Kinesis":            {KinesisEnvelope, `{"Records":[{"kinesis":{"data":"//4="}}]}`},
	}
	for name, tc := range rawCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse[any](tc.raw, WithEnvelope(tc.envelope))
			require.ErrorIs(t, err, ErrParse)
		})
	}
}

func TestKafkaOrder(t *testing.T) {
	t.Parallel()
	value := func(n int) string { return base64.StdEncoding.EncodeToString([]byte(`{"n":` + strconv.Itoa(n) + `}`)) }
	raw := `{"eventSource":"SelfManagedKafka","records":{
		"z-0":[{"value":"` + value(1) + `"},{"value":"` + value(2) + `"}],
		"a-0":[{"value":"` + value(3) + `"},{"value":null}]}}`
	type n struct {
		N int `json:"n"`
	}

	inOrder, err := Parse[[]*n](raw, WithEnvelope(KafkaEnvelope))
	require.NoError(t, err)
	require.Len(t, inOrder, 4)
	assert.Equal(t, []int{1, 2, 3}, []int{inOrder[0].N, inOrder[1].N, inOrder[2].N})
	assert.Nil(t, inOrder[3], "a tombstone has no payload")

	var event events.KafkaEvent
	require.NoError(t, json.Unmarshal([]byte(raw), &event))
	sorted, err := Kafka[*n](event)
	require.NoError(t, err)
	require.Len(t, sorted, 4)
	assert.Equal(t, []int{3, 1, 2}, []int{sorted[0].N, sorted[2].N, sorted[3].N})
	assert.Nil(t, sorted[1])

	self, err := Parse[[]keyValue](sample(t, "kafkaEventSelfManaged.json"), WithEnvelope(KafkaEnvelope))
	require.NoError(t, err)
	assert.NotEmpty(t, self)
}

func TestDynamoDBStream(t *testing.T) {
	t.Parallel()
	type item struct {
		ID      int    `json:"Id"`
		Message string `json:"Message"`
	}
	stream := decodeSample[events.DynamoDBEvent](t, "dynamoStreamEvent.json")
	want := []DynamoDBImages[item]{
		{NewImage: &item{ID: 101, Message: "New item!"}},
		{NewImage: &item{ID: 101, Message: "This item has changed"}, OldImage: &item{ID: 101, Message: "New item!"}},
	}

	typed, err := DynamoDBStream[item](stream)
	require.NoError(t, err)
	assert.Equal(t, want, typed)

	raw, err := Parse[[]DynamoDBImages[item]](sample(t, "dynamoStreamEvent.json"), WithEnvelope(DynamoDBStreamEnvelope))
	require.NoError(t, err)
	assert.Equal(t, want, raw)

	_, err = DynamoDBStream[message](stream)
	require.ErrorIs(t, err, errInvalid, "the images have no message member")
	_, err = Parse[[]DynamoDBImages[message]](sample(t, "dynamoStreamEvent.json"), WithEnvelope(DynamoDBStreamEnvelope))
	require.ErrorIs(t, err, errInvalid)
	assert.Contains(t, err.Error(), "NewImage")

	bad := events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{{Change: events.DynamoDBStreamRecord{
		OldImage: map[string]events.DynamoDBAttributeValue{"n": events.NewNumberAttribute("x")},
	}}}}
	_, err = DynamoDBStream[item](bad)
	require.ErrorIs(t, err, ErrInvalidPayload)
	bad.Records[0].Change.NewImage = bad.Records[0].Change.OldImage
	_, err = DynamoDBStream[item](bad)
	require.ErrorIs(t, err, ErrInvalidPayload)
	_, err = DynamoDBStream[item](events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{{Change: events.DynamoDBStreamRecord{
		NewImage: map[string]events.DynamoDBAttributeValue{"Id": events.NewStringAttribute("x")},
	}}}})
	require.ErrorIs(t, err, ErrParse)
}

func TestEventParser(t *testing.T) {
	t.Parallel()

	handler := EventParser(func(_ context.Context, orders []message) (int, error) {
		return len(orders), nil
	}, WithEnvelope(SNSSQSEnvelope))
	n, err := handler(t.Context(), sample(t, "snsSqsEvent.json"))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	called := false
	strict := EventParser(func(_ context.Context, m message) (string, error) {
		called = true
		return m.Message, nil
	})
	_, err = strict(t.Context(), jsontext.Value(`{"message":""}`))
	require.ErrorIs(t, err, ErrParse)
	assert.False(t, called, "the handler does not run on invalid events")

	out, err := strict(t.Context(), jsontext.Value(`{"message":"hi"}`))
	require.NoError(t, err)
	assert.Equal(t, "hi", out)
}
