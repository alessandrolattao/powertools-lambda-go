package parser

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzEnvelopes feeds arbitrary JSON to every envelope: whatever an event
// carries, parsing returns a value or an error, never panics.
func FuzzEnvelopes(f *testing.F) {
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
	envelopes := []Envelope{
		APIGatewayRESTEnvelope, APIGatewayHTTPEnvelope, APIGatewayWebSocketEnvelope, FunctionURLEnvelope,
		VPCLatticeEnvelope, VPCLatticeV2Envelope, EventBridgeEnvelope, SQSEnvelope, SNSEnvelope, SNSSQSEnvelope,
		CloudWatchLogsEnvelope, KinesisEnvelope, FirehoseEnvelope, KafkaEnvelope, DynamoDBStreamEnvelope,
		BedrockAgentEnvelope, BedrockAgentFunctionEnvelope,
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		for _, envelope := range envelopes {
			unwrapped, err := envelope.Unwrap(raw)
			if err == nil && !unwrapped.IsValid() {
				t.Fatalf("%T unwrapped to invalid JSON %q", envelope, unwrapped)
			}
			_, _ = Parse[any](raw, WithEnvelope(envelope))       //nolint:errcheck // the property is the absence of panics
			_, _ = Parse[[]message](raw, WithEnvelope(envelope)) //nolint:errcheck // the property is the absence of panics
		}
		_, _ = Parse[JSON[map[string]any]](raw) //nolint:errcheck // the property is the absence of panics
	})
}
