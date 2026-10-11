package eventsource

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouteARN(t *testing.T) {
	t.Parallel()

	arn, err := ParseRouteARN("arn:aws-cn:execute-api:cn-north-1:123456789012:abcdef123/test/GET/pets/{id}")
	require.NoError(t, err)
	assert.Equal(t, RouteARN{
		Partition: "aws-cn", Region: "cn-north-1", AccountID: "123456789012", APIID: "abcdef123",
		Stage: "test", Method: HTTPVerbGet, Resource: "pets/{id}",
	}, arn)
	assert.Equal(t, "arn:aws-cn:execute-api:cn-north-1:123456789012:abcdef123/test/GET/pets/{id}", arn.String())

	root, err := ParseRouteARN("arn:aws:execute-api:us-east-1:123456789012:abcdef123/test/GET")
	require.NoError(t, err)
	assert.Empty(t, root.Resource)

	ws, err := ParseWebSocketRouteARN("arn:aws:execute-api:us-east-1:123456789012:abcdef123/prod/$connect")
	require.NoError(t, err)
	assert.Equal(t, "$connect", ws.Resource)
	assert.Empty(t, ws.Method)
	assert.Equal(t, "arn:aws:execute-api:us-east-1:123456789012:abcdef123/prod/$connect", ws.String())

	for _, bad := range []string{"", "arn:aws:execute-api", "arn:aws:execute-api:r:a:api", "nope:aws:execute-api:r:a:api/s/GET"} {
		_, err := ParseRouteARN(bad)
		require.ErrorIs(t, err, ErrInvalidRouteARN, bad)
	}
}

func TestAuthorizerResponse(t *testing.T) {
	t.Parallel()
	target, err := ParseRouteARN("arn:aws:execute-api:us-east-1:123456789012:api1/prod/GET/")
	require.NoError(t, err)

	response := NewAuthorizerResponse("user-1", target,
		WithAuthorizerContext(map[string]any{"tenant": "t1"}), WithUsageIdentifierKey("key-1"))
	require.NoError(t, response.AllowRoute(HTTPVerbGet, "/pets"))
	require.NoError(t, response.AllowRoute("ALL", "/admin/*",
		Condition{"IpAddress": {"aws:SourceIp": []string{"10.0.0.0/8"}}},
		Condition{"IpAddress": {"aws:VpcSourceIp": "10.0.0.1"}, "Bool": {"aws:SecureTransport": "true"}},
	))
	require.NoError(t, response.DenyRoute(HTTPVerbDelete, "/pets/{id}"))
	response.DenyAllRoutes()

	built, err := response.Build()
	require.NoError(t, err)
	out, err := json.Marshal(built)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"principalId": "user-1",
		"policyDocument": {
			"Version": "2012-10-17",
			"Statement": [
				{"Action": "execute-api:Invoke", "Effect": "Allow",
				 "Resource": ["arn:aws:execute-api:us-east-1:123456789012:api1/prod/*/admin/*"],
				 "Condition": {"IpAddress": {"aws:SourceIp": ["10.0.0.0/8"], "aws:VpcSourceIp": "10.0.0.1"}, "Bool": {"aws:SecureTransport": "true"}}},
				{"Action": "execute-api:Invoke", "Effect": "Allow",
				 "Resource": ["arn:aws:execute-api:us-east-1:123456789012:api1/prod/GET/pets"]},
				{"Action": "execute-api:Invoke", "Effect": "Deny",
				 "Resource": ["arn:aws:execute-api:us-east-1:123456789012:api1/prod/DELETE/pets/{id}",
				              "arn:aws:execute-api:us-east-1:123456789012:api1/prod/*/*"]}
			]
		},
		"context": {"tenant": "t1"},
		"usageIdentifierKey": "key-1"
	}`, string(out))

	require.ErrorIs(t, response.AllowRoute("FETCH", "/pets"), ErrInvalidHTTPVerb)
	require.ErrorIs(t, response.AllowRoute(HTTPVerbGet, "/pets?x=1"), ErrInvalidResourcePath)

	_, err = NewAuthorizerResponse("nobody", target).Build()
	require.ErrorIs(t, err, ErrNoStatements)

	all := NewAuthorizerResponse("user-1", RouteARN{Region: "eu-west-1", AccountID: "1", APIID: "a", Stage: "s"})
	all.AllowAllRoutes()
	built, err = all.Build()
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:aws:execute-api:eu-west-1:1:a/s/*/*"}, built.PolicyDocument.Statement[0].Resource)
}

func TestWebSocketAuthorizerResponse(t *testing.T) {
	t.Parallel()
	target, err := ParseWebSocketRouteARN("arn:aws:execute-api:us-east-1:123456789012:ws1/prod/$connect")
	require.NoError(t, err)
	response := NewWebSocketAuthorizerResponse("user-1", target)
	response.AllowRoute("$connect")
	response.DenyRoute("admin", Condition{"Bool": {"aws:SecureTransport": "false"}})
	response.AllowAllRoutes()
	response.DenyAllRoutes()
	built, err := response.Build()
	require.NoError(t, err)
	require.Len(t, built.PolicyDocument.Statement, 3)
	assert.Equal(t, []string{
		"arn:aws:execute-api:us-east-1:123456789012:ws1/prod/$connect",
		"arn:aws:execute-api:us-east-1:123456789012:ws1/prod/*",
	}, built.PolicyDocument.Statement[0].Resource)
	assert.Equal(t, "Deny", built.PolicyDocument.Statement[1].Effect)
	assert.NotNil(t, built.PolicyDocument.Statement[1].Condition)

	_, err = NewWebSocketAuthorizerResponse("nobody", target).Build()
	require.ErrorIs(t, err, ErrNoStatements)
}

func TestDenyAllResponse(t *testing.T) {
	t.Parallel()
	out, err := json.Marshal(DenyAllResponse())
	require.NoError(t, err)
	assert.JSONEq(t, `{"principalId":"deny-all-user","policyDocument":{"Version":"2012-10-17",
		"Statement":[{"Action":"execute-api:Invoke","Effect":"Deny","Resource":["*"]}]}}`, string(out))
}

func TestDynamoDBImage(t *testing.T) {
	t.Parallel()
	var image map[string]events.DynamoDBAttributeValue
	require.NoError(t, json.Unmarshal([]byte(`{
		"id": {"N": "12345678901234567890123456789012345678"},
		"price": {"N": "1.50"},
		"name": {"S": "widget"},
		"blob": {"B": "aGVsbG8="},
		"active": {"BOOL": true},
		"gone": {"NULL": true},
		"tags": {"SS": ["a", "b"]},
		"sizes": {"NS": ["1", "2.5"]},
		"blobs": {"BS": ["aGk="]},
		"list": {"L": [{"S": "x"}, {"N": "3"}]},
		"map": {"M": {"nested": {"N": "-0.5"}}}
	}`), &image))

	values, err := DynamoDBImage(image)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"id":     Number("12345678901234567890123456789012345678"),
		"price":  Number("1.50"),
		"name":   "widget",
		"blob":   []byte("hello"),
		"active": true,
		"gone":   nil,
		"tags":   []string{"a", "b"},
		"sizes":  []Number{"1", "2.5"},
		"blobs":  [][]byte{[]byte("hi")},
		"list":   []any{"x", Number("3")},
		"map":    map[string]any{"nested": Number("-0.5")},
	}, values)

	type item struct {
		ID     Number            `json:"id"`
		Price  float64           `json:"price"`
		Name   string            `json:"name"`
		Blob   []byte            `json:"blob"`
		Active bool              `json:"active"`
		Tags   []string          `json:"tags"`
		Sizes  []float64         `json:"sizes"`
		Map    map[string]Number `json:"map"`
	}
	got, err := DecodeDynamoDBImage[item](image)
	require.NoError(t, err)
	assert.Equal(t, item{
		ID: "12345678901234567890123456789012345678", Price: 1.5, Name: "widget", Blob: []byte("hello"),
		Active: true, Tags: []string{"a", "b"}, Sizes: []float64{1, 2.5}, Map: map[string]Number{"nested": "-0.5"},
	}, got)

	raw, err := DynamoDBImageJSON(map[string]events.DynamoDBAttributeValue{"n": events.NewNumberAttribute("7")})
	require.NoError(t, err)
	assert.JSONEq(t, `{"n":7}`, string(raw))

	empty, err := DynamoDBImage(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	stream := decode[events.DynamoDBEvent](t, "dynamoStreamEvent.json")
	type message struct {
		ID      int    `json:"Id"`
		Message string `json:"Message"`
	}
	newImage, err := DecodeDynamoDBImage[message](stream.Records[0].Change.NewImage)
	require.NoError(t, err)
	assert.Equal(t, message{ID: 101, Message: "New item!"}, newImage)

	_, err = DynamoDBImage(map[string]events.DynamoDBAttributeValue{"bad": events.NewNumberAttribute("1x")})
	require.ErrorIs(t, err, ErrInvalidNumber)
	_, err = DecodeDynamoDBImage[message](map[string]events.DynamoDBAttributeValue{"Id": events.NewStringAttribute("x")})
	require.Error(t, err)
}

func TestNumber(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"", "0", false},
		{".", "0", false},
		{"+5", "5", false},
		{".5", "0.5", false},
		{"-.5", "-0.5", false},
		{"5.", "5", false},
		{"1.e5", "1e5", false},
		{"1E+2", "1E+2", false},
		{"1e", "", true},
		{"abc", "", true},
		{"-", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeNumber(tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidNumber)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	n := Number("42")
	i, err := n.Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(42), i)
	f, err := Number("2.5").Float64()
	require.NoError(t, err)
	assert.InDelta(t, 2.5, f, 0)
	_, err = Number("2.5").Int64()
	require.ErrorIs(t, err, ErrInvalidNumber)
	_, err = Number("x").Float64()
	require.ErrorIs(t, err, ErrInvalidNumber)
	assert.Equal(t, "42", n.String())

	var parsed struct {
		A Number `json:"a"`
		B Number `json:"b"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"a":1.25,"b":"7"}`), &parsed))
	assert.Equal(t, Number("1.25"), parsed.A)
	assert.Equal(t, Number("7"), parsed.B)
	require.Error(t, json.Unmarshal([]byte(`{"a":"x"}`), &parsed))
}

func TestSQS(t *testing.T) {
	t.Parallel()

	dlq := decode[events.SQSEvent](t, "sqsDlqTriggerEvent.json")
	attrs, err := ParseSQSAttributes(dlq.Records[0])
	require.NoError(t, err)
	assert.Equal(t, 2, attrs.ApproximateReceiveCount)
	assert.NotEmpty(t, attrs.DeadLetterQueueSourceARN)
	assert.False(t, attrs.SentTimestamp.IsZero())
	assert.Equal(t, time.UTC, attrs.SentTimestamp.Location())

	_, err = ParseSQSAttributes(events.SQSMessage{Attributes: map[string]string{SQSAttributeApproximateReceiveCount: "x"}})
	require.Error(t, err)
	_, err = ParseSQSAttributes(events.SQSMessage{Attributes: map[string]string{SQSAttributeSentTimestamp: "x"}})
	require.Error(t, err)

	url, err := SQSQueueURL(dlq.Records[0].EventSourceARN)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(url, "https://sqs."))

	cases := []struct{ arn, want string }{
		{"arn:aws:sqs:us-east-1:123456789012:orders", "https://sqs.us-east-1.amazonaws.com/123456789012/orders"},
		{"arn:aws-cn:sqs:cn-north-1:123456789012:orders", "https://sqs.cn-north-1.amazonaws.com.cn/123456789012/orders"},
		{"arn:aws-us-gov:sqs:us-gov-west-1:123456789012:orders.fifo", "https://sqs.us-gov-west-1.amazonaws.com/123456789012/orders.fifo"},
		{"arn:aws-new:sqs:xx-1:1:q", "https://sqs.xx-1.amazonaws.com/1/q"},
	}
	for _, tc := range cases {
		got, urlErr := SQSQueueURL(tc.arn)
		require.NoError(t, urlErr)
		assert.Equal(t, tc.want, got)
	}
	_, err = SQSQueueURL("arn:aws:sns:us-east-1:1:topic")
	require.ErrorIs(t, err, ErrInvalidQueueARN)

	s3 := decode[events.SQSEvent](t, "s3SqsEvent.json")
	nestedS3, err := NestedS3Event(s3.Records[0])
	require.NoError(t, err)
	require.NotEmpty(t, nestedS3.Records)
	assert.NotEmpty(t, nestedS3.Records[0].S3.Bucket.Name)

	for _, name := range []string{"snsSqsEvent.json", "snsSqsFifoEvent.json"} {
		sns := decode[events.SQSEvent](t, name)
		nested, nestedErr := NestedSNSMessage(sns.Records[0])
		require.NoError(t, nestedErr)
		assert.NotEmpty(t, nested.TopicArn)
		assert.NotEmpty(t, nested.Message)
		if name == "snsSqsEvent.json" {
			assert.NotEmpty(t, nested.UnsubscribeURL, "the SQS spelling UnsubscribeURL decodes")
			assert.NotEmpty(t, nested.SigningCertURL)
		}
	}

	_, err = NestedS3Event(events.SQSMessage{Body: "nope"})
	require.Error(t, err)
	_, err = NestedSNSMessage(events.SQSMessage{Body: "nope"})
	require.Error(t, err)
}

func TestSNSMessageAttributes(t *testing.T) {
	t.Parallel()
	sns := decode[events.SNSEvent](t, "snsEvent.json")
	attrs, err := SNSMessageAttributes(sns.Records[0].SNS)
	require.NoError(t, err)
	assert.Equal(t, SNSMessageAttribute{Type: "String", Value: "TestString"}, attrs["Test"])

	empty, err := SNSMessageAttributes(events.SNSEntity{})
	require.NoError(t, err)
	assert.Empty(t, empty)
	_, err = SNSMessageAttributes(events.SNSEntity{MessageAttributes: map[string]any{"bad": 1}})
	require.Error(t, err)
}

func TestUnquoteS3Key(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain.txt":           "plain.txt",
		"my+file.txt":         "my file.txt",
		"a%2Bb%20c":           "a+b c",
		"bad%zzescape%":       "bad%zzescape%",
		"%E2%9C%93check":      "✓check",
		"%FFinvalid":          "�invalid",
		"trailing%2":          "trailing%2",
		"photos/2024/img.jpg": "photos/2024/img.jpg",
	}
	for in, want := range cases {
		assert.Equal(t, want, UnquoteS3Key(in), in)
	}
}

func TestS3Batch(t *testing.T) {
	t.Parallel()

	v1 := decode[events.S3BatchJobEvent](t, "s3BatchOperationEventSchemaV1.json")
	assert.Equal(t, "powertools-dataset", S3BatchTaskBucket(v1.Tasks[0]))
	assert.Equal(t, UnquoteS3Key(v1.Tasks[0].S3Key), S3BatchTaskKey(v1.Tasks[0]))
	v2 := decode[events.S3BatchJobEventV2](t, "s3BatchOperationEventSchemaV2.json")
	assert.Equal(t, "powertools-dataset", S3BatchTaskBucket(v2.Tasks[0]))
	assert.NotEmpty(t, S3BatchTaskKey(v2.Tasks[0]))
	assert.Equal(t, "plain-bucket", S3BucketFromARN("plain-bucket"))

	response := NewS3BatchJobResponse(v2.InvocationSchemaVersion, v2.InvocationID, events.S3BatchJobResult{
		TaskID: v2.Tasks[0].TaskID, ResultCode: S3BatchSucceeded, ResultString: "done",
	})
	assert.Equal(t, S3BatchSucceeded, response.TreatMissingKeysAs)
	require.NoError(t, ValidateS3BatchJobResponse(response))

	require.ErrorIs(t, ValidateS3BatchJobResponse(NewS3BatchJobResponse("2.0", "id")), ErrEmptyResponse)
	two := NewS3BatchJobResponse("2.0", "id", events.S3BatchJobResult{ResultCode: S3BatchSucceeded}, events.S3BatchJobResult{ResultCode: S3BatchSucceeded})
	require.ErrorIs(t, ValidateS3BatchJobResponse(two), ErrInvalidResult)
	bad := NewS3BatchJobResponse("2.0", "id", events.S3BatchJobResult{ResultCode: "Maybe"})
	require.ErrorIs(t, ValidateS3BatchJobResponse(bad), ErrInvalidResult)
}

func TestFirehoseResponse(t *testing.T) {
	t.Parallel()
	var response KinesisFirehoseResponse
	require.ErrorIs(t, response.Validate(), ErrEmptyResponse)

	response.Add(
		KinesisFirehoseResponseRecord{RecordID: "1", Result: events.KinesisFirehoseTransformedStateOk, Data: []byte("hi")},
		KinesisFirehoseResponseRecord{
			RecordID: "2", Result: events.KinesisFirehoseTransformedStateDropped,
			Metadata: &KinesisFirehoseResponseRecordMetadata{PartitionKeys: map[string]string{"year": "2024"}},
		},
	)
	require.NoError(t, response.Validate())
	out, err := json.Marshal(response)
	require.NoError(t, err)
	assert.JSONEq(t, `{"records":[
		{"recordId":"1","result":"Ok","data":"aGk="},
		{"recordId":"2","result":"Dropped","data":"","metadata":{"partitionKeys":{"year":"2024"}}}
	]}`, string(out))

	response.Add(KinesisFirehoseResponseRecord{RecordID: "3", Result: "Maybe"})
	require.ErrorIs(t, response.Validate(), ErrInvalidResult)
}

func TestTransferFamilyResponse(t *testing.T) {
	t.Parallel()

	s3, err := NewTransferFamilyS3Response("arn:aws:iam::1:role/r", WithHomeDirectory("/bucket/home"),
		WithSessionPolicy(`{"Version":"2012-10-17"}`), WithPublicKeys("ssh-rsa AAAA"))
	require.NoError(t, err)
	out, err := json.Marshal(s3)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Role":"arn:aws:iam::1:role/r","HomeDirectoryType":"PATH","HomeDirectory":"/bucket/home",
		"Policy":"{\"Version\":\"2012-10-17\"}","PublicKeys":["ssh-rsa AAAA"]}`, string(out))

	efs, err := NewTransferFamilyEFSResponse("arn:aws:iam::1:role/r", 1000, 2000,
		WithLogicalHomeDirectory(TransferFamilyDirectoryMapping{Entry: "/", Target: "/fs-1/home"}))
	require.NoError(t, err)
	assert.Equal(t, TransferFamilyHomeDirectoryLogical, efs.HomeDirectoryType)
	assert.JSONEq(t, `[{"Entry":"/","Target":"/fs-1/home"}]`, efs.HomeDirectoryDetails)
	assert.Equal(t, &TransferFamilyPosixProfile{UID: 1000, GID: 2000}, efs.PosixProfile)

	_, err = NewTransferFamilyS3Response("role")
	require.ErrorIs(t, err, ErrInvalidHomeDirectory)
	_, err = NewTransferFamilyS3Response("role", WithLogicalHomeDirectory())
	require.ErrorIs(t, err, ErrInvalidHomeDirectory)
}

func TestCodePipeline(t *testing.T) {
	t.Parallel()

	job := decode[CodePipelineJobEvent](t, "codePipelineEventData.json")
	assert.Equal(t, "c0d76431-b0e7-xmpl-97e3-e8ee786eb6f6", job.CodePipelineJob.ID)
	assert.JSONEq(t, `{"KEY": "VALUE"}`, job.UserParameters())
	params, err := job.DecodeUserParameters[map[string]string]()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"KEY": "VALUE"}, params)
	assert.Equal(t, "us-west-2-123456789012-my-pipeline", job.InputBucketName())
	assert.Equal(t, "my-pipeline/test-api-2/TdOSFRV", job.InputObjectKey())
	require.NotNil(t, job.CodePipelineJob.Data.EncryptionKey)
	assert.Equal(t, CodePipelineEncryptionKey{ID: "someKey", Type: "KMS"}, *job.CodePipelineJob.Data.EncryptionKey)
	assert.Equal(t, int64(1575493418000), job.CodePipelineJob.Data.ArtifactCredentials.ExpirationTime)
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", job.CodePipelineJob.Data.ArtifactCredentials.AccessKeyID)

	input, ok := job.FindInputArtifact("my-pipeline-SourceArtifact")
	assert.True(t, ok)
	assert.Equal(t, "S3", input.Location.LocationType)
	_, ok = job.FindInputArtifact("missing")
	assert.False(t, ok)
	output, ok := job.FindOutputArtifact("invokeOutput")
	assert.True(t, ok)
	assert.Nil(t, output.Revision)
	_, ok = job.FindOutputArtifact("missing")
	assert.False(t, ok)

	plain := decode[CodePipelineJobEvent](t, "codePipelineEvent.json")
	assert.Nil(t, plain.CodePipelineJob.Data.EncryptionKey)
	_, err = plain.DecodeUserParameters[map[string]string]()
	require.Error(t, err, "the sample's user parameters are not JSON")

	empty := decode[CodePipelineJobEvent](t, "codePipelineEventEmptyUserParameters.json")
	none, err := empty.DecodeUserParameters[map[string]string]()
	require.NoError(t, err)
	assert.Nil(t, none)
	assert.Empty(t, CodePipelineJobEvent{}.InputBucketName())
	assert.Empty(t, CodePipelineJobEvent{}.InputObjectKey())

	keyed := decode[CodePipelineJobEvent](t, "codePipelineEventWithEncryptionKey.json")
	assert.Equal(t, "validkmskey", keyed.CodePipelineJob.Data.EncryptionKey.ID)
}

func TestCodePipelineCredentialsAreRedacted(t *testing.T) {
	t.Parallel()
	job := decode[CodePipelineJobEvent](t, "codePipelineEventData.json")
	creds := job.CodePipelineJob.Data.ArtifactCredentials

	for _, rendered := range []string{
		fmt.Sprintf("%v", job), fmt.Sprintf("%+v", job), fmt.Sprintf("%#v", creds), creds.String(),
	} {
		assert.NotContains(t, rendered, "6CGtmAa3lzWtV7a")
		assert.NotContains(t, rendered, "IQoJb3JpZ2luX2VjEA")
		assert.Contains(t, rendered, sensitive)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("job", "credentials", creds)
	assert.NotContains(t, buf.String(), "6CGtmAa3lzWtV7a")
	assert.Contains(t, buf.String(), "AKIAIOSFODNN7EXAMPLE")
}

func TestDecompress(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"messageType":"DATA_MESSAGE"}`)

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, err := gw.Write(payload)
	require.NoError(t, err)
	require.NoError(t, gw.Close())

	var zl bytes.Buffer
	zw := zlib.NewWriter(&zl)
	_, err = zw.Write(payload)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	for name, data := range map[string][]byte{"gzip": gz.Bytes(), "zlib": zl.Bytes()} {
		got, decompressErr := Decompress(data)
		require.NoError(t, decompressErr, name)
		assert.Equal(t, payload, got, name)
	}
	_, err = Decompress([]byte("plain text"))
	require.ErrorIs(t, err, ErrNotCompressed)
	_, err = Decompress(gz.Bytes()[:12])
	require.Error(t, err)
	_, err = Decompress([]byte{0x1f, 0x8b, 0, 0})
	require.Error(t, err)
}

func TestExtractCloudWatchLogs(t *testing.T) {
	t.Parallel()
	stream := decode[events.KinesisEvent](t, "kinesisStreamCloudWatchLogsEvent.json")
	logs, err := ExtractCloudWatchLogs(stream)
	require.NoError(t, err)
	require.Len(t, logs, len(stream.Records))
	assert.Equal(t, "231436140809", logs[0].Owner)
	assert.NotEmpty(t, logs[0].LogEvents)

	stream.Records[0].Kinesis.Data = []byte("not compressed")
	_, err = ExtractCloudWatchLogs(stream)
	require.ErrorIs(t, err, ErrNotCompressed)
	_, err = ParseCloudWatchLogs(events.CloudwatchLogsRawData{Data: "!!"})
	require.Error(t, err)
}

func TestAppSyncScalars(t *testing.T) {
	t.Parallel()
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}Z$`, AWSDate(0))
	assert.Regexp(t, `^\d{2}:\d{2}:\d{2}\.\d{3}\+05:00:00$`, AWSTime(5))
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}-11:00:00$`, AWSDateTime(-11))
	assert.InDelta(t, time.Now().Unix(), AWSTimestamp(), 2)
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, MakeID())

	at := time.Date(2024, 1, 31, 23, 30, 0, 123456789, time.UTC)
	assert.Equal(t, "2024-02-01T01:30:00.123+02:00:00", formatAWSTime(at, "2006-01-02T15:04:05.000", 2))
	assert.Equal(t, "2024-01-31T13:30:00.123-10:00:00", formatAWSTime(at, "2006-01-02T15:04:05.000", -10))
}

func TestStringValuesAndEpoch(t *testing.T) {
	t.Parallel()
	var values struct {
		A StringValues `json:"a"`
		B StringValues `json:"b"`
		C StringValues `json:"c"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"a":"x","b":["y","z"],"c":null}`), &values))
	assert.Equal(t, StringValues{"x"}, values.A)
	assert.Equal(t, StringValues{"y", "z"}, values.B)
	assert.Nil(t, values.C)
	require.Error(t, json.Unmarshal([]byte(`{"a":1}`), &values))

	var epochs struct {
		A EpochMicros `json:"a"`
		B EpochMicros `json:"b"`
		C EpochMicros `json:"c"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"a":"1696331543569073","b":1696331543569073,"c":null}`), &epochs))
	assert.Equal(t, epochs.A, epochs.B)
	assert.Zero(t, epochs.C)
	require.Error(t, json.Unmarshal([]byte(`{"a":"soon"}`), &epochs))
}
