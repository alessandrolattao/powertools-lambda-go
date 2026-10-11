package eventsource

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
)

// addSamples seeds a fuzz target with every sample event.
func addSamples(f *testing.F) {
	f.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		f.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", entry.Name()))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
}

// FuzzDecode feeds arbitrary JSON to every event type of the package and to
// the helpers that read them: whatever an event carries, they return a value
// or an error, never panic.
func FuzzDecode(f *testing.F) {
	addSamples(f)
	f.Fuzz(func(_ *testing.T, raw []byte) {
		decodeAll(raw)

		var config ConfigEvent
		if json.Unmarshal(raw, &config) == nil {
			_, _ = config.DecodeInvokingEvent()                  //nolint:errcheck // the property is the absence of panics
			_, _ = config.DecodeRuleParameters[map[string]any]() //nolint:errcheck // the property is the absence of panics
		}
		var pipeline CodePipelineJobEvent
		if json.Unmarshal(raw, &pipeline) == nil {
			_, _ = pipeline.DecodeUserParameters[map[string]any]() //nolint:errcheck // the property is the absence of panics
			_ = pipeline.InputObjectKey()
		}
		var alarm CloudWatchAlarmEvent
		if json.Unmarshal(raw, &alarm) == nil {
			_, _ = alarm.AlarmData.State.DecodeReasonData[any]() //nolint:errcheck // the property is the absence of panics
			_, _ = ParseCloudWatchTime(alarm.Time)               //nolint:errcheck // the property is the absence of panics
		}
		var lattice VPCLatticeEventV2
		if json.Unmarshal(raw, &lattice) == nil {
			exerciseHTTP(lattice)
		}
		var rest events.APIGatewayProxyRequest
		if json.Unmarshal(raw, &rest) == nil {
			exerciseHTTP(rest)
		}
		var sqs events.SQSEvent
		if json.Unmarshal(raw, &sqs) == nil {
			for i := range sqs.Records {
				m := &sqs.Records[i]
				_, _ = ParseSQSAttributes(*m)        //nolint:errcheck // the property is the absence of panics
				_, _ = NestedS3Event(*m)             //nolint:errcheck // the property is the absence of panics
				_, _ = NestedSNSMessage(*m)          //nolint:errcheck // the property is the absence of panics
				_, _ = SQSQueueURL(m.EventSourceARN) //nolint:errcheck // the property is the absence of panics
			}
		}
		var logs events.CloudwatchLogsEvent
		if json.Unmarshal(raw, &logs) == nil {
			_, _ = ParseCloudWatchLogs(logs.AWSLogs) //nolint:errcheck // the property is the absence of panics
		}
	})
}

func decodeAll(raw []byte) {
	targets := []any{
		new(VPCLatticeEvent), new(VPCLatticeEventV2), new(APIGatewayProxyRequest),
		new(APIGatewayCustomAuthorizerRequestTypeRequest), new(AppSyncResolverEvent), new(AppSyncEventsEvent),
		new(BedrockAgentEvent), new(BedrockAgentFunctionEvent), new(CloudWatchAlarmEvent),
		new(CloudWatchDashboardCustomWidgetEvent), new(CloudWatchLogsData), new(CodeDeployLifecycleHookEvent),
		new(CodePipelineJobEvent), new(CognitoEventUserPoolsCustomSender), new(CognitoEventUserPoolsMigrateUser),
		new(CognitoEventUserPoolsCustomMessage), new(ConfigEvent), new(ConnectEvent), new(EventBridgeEvent),
		new(S3EventBridgeNotificationEvent), new(IoTCoreThingEvent), new(IoTCoreThingTypeEvent),
		new(IoTCoreThingTypeAssociationEvent), new(IoTCoreThingGroupEvent), new(IoTCoreAddOrRemoveFromThingGroupEvent),
		new(IoTCoreAddOrDeleteFromThingGroupEvent), new(KinesisTimeWindowEvent), new(DynamoDBTimeWindowEvent),
		new(ActiveMQEvent), new(S3ObjectLambdaEvent), new(SimpleEmailEvent), new(TransferFamilyAuthorizerEvent),
		new(CloudFormationCustomResourceEvent), new(KinesisFirehoseResponse),
	}
	for _, target := range targets {
		_ = json.Unmarshal(raw, target) //nolint:errcheck // the property is the absence of panics
	}
}

func exerciseHTTP[E HTTPRequest](e E) {
	_, _ = Body(e)          //nolint:errcheck // the property is the absence of panics
	_, _ = JSONBody[any](e) //nolint:errcheck // the property is the absence of panics
	_, _ = Header(e, "Content-Type")
	_ = HeaderValues(e, "accept")
	_, _ = QueryParameter(e, "id")
	_ = QueryParameterValues(e, "id")
	_ = Cookies(e)
	_ = Path(e)
	_ = Method(e)
}

// FuzzDecompress checks that arbitrary bytes never make the decompressor
// panic or inflate past its limit.
func FuzzDecompress(f *testing.F) {
	f.Add([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff})
	f.Add([]byte{0x78, 0x9c, 3, 0, 0, 0, 0, 1})
	f.Add([]byte("plain"))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := Decompress(data)
		if err == nil && len(out) > MaxDecompressedSize {
			t.Fatalf("inflated %d bytes past the limit", len(out))
		}
	})
}

// FuzzDynamoDB feeds arbitrary attribute values to the image conversions.
func FuzzDynamoDB(f *testing.F) {
	f.Add([]byte(`{"a":{"N":"1.5"},"b":{"L":[{"S":"x"},{"M":{"c":{"NS":["1","2"]}}}]},"d":{"B":"aGk="}}`))
	f.Add([]byte(`{"a":{"NULL":true},"b":{"BOOL":false},"c":{"SS":["x"]},"d":{"BS":["aGk="]}}`))
	f.Fuzz(func(_ *testing.T, raw []byte) {
		var image map[string]events.DynamoDBAttributeValue
		if json.Unmarshal(raw, &image) != nil {
			return
		}
		_, _ = DynamoDBImage(image)                       //nolint:errcheck // the property is the absence of panics
		_, _ = DecodeDynamoDBImage[map[string]any](image) //nolint:errcheck // the property is the absence of panics
	})
}

// FuzzStrings feeds arbitrary text to the parsers of strings found in events.
func FuzzStrings(f *testing.F) {
	f.Add("a%2Bb+c%zz", "arn:aws:execute-api:us-east-1:1:api/stage/GET/pets/{id}", "a=1; b", "1.e5")
	f.Fuzz(func(t *testing.T, key, arn, cookies, number string) {
		if !utf8.ValidString(UnquoteS3Key(key)) {
			t.Fatalf("UnquoteS3Key(%q) is not UTF-8", key)
		}
		if route, err := ParseRouteARN(arn); err == nil {
			_ = route.String()
		}
		_, _ = ParseWebSocketRouteARN(arn) //nolint:errcheck // the property is the absence of panics
		_, _ = SQSQueueURL(arn)            //nolint:errcheck // the property is the absence of panics
		_ = ParseCookies(cookies)
		if normalized, err := normalizeNumber(number); err == nil {
			var v any
			if json.Unmarshal([]byte(normalized), &v) != nil {
				t.Fatalf("normalizeNumber(%q) = %q, not JSON", number, normalized)
			}
		}
	})
}
