package bedrockagent_test

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/bedrockagent"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

func FuzzAgent(f *testing.F) {
	f.Add(`{"httpMethod":"GET","apiPath":"/claims/{claim_id}","parameters":[{"name":"claim_id","type":"string","value":"a/b"}]}`)
	f.Add(`{"httpMethod":"POST","apiPath":"/send-reminders","requestBody":{"content":{"application/json":{"properties":[{"name":"claimId","type":"integer","value":"7"},{"name":"channels","type":"array","value":"[1"}]}}}}`)
	f.Add(`{"httpMethod":"get","apiPath":"claims"}`)
	handler := bedrockagent.Handler(agentApp())
	f.Fuzz(func(t *testing.T, event string) {
		var e eventsource.BedrockAgentEvent
		if json.Unmarshal([]byte(event), &e) != nil {
			return
		}
		resp, err := handler(t.Context(), e)
		if err != nil && err.Error() != "boom" {
			t.Fatalf("event %s: %v", event, err)
		}
		if err == nil && resp.MessageVersion != bedrockagent.MessageVersion {
			t.Fatalf("message version %q", resp.MessageVersion)
		}
	})
}

func FuzzFunctions(f *testing.F) {
	f.Add(`{"function":"typed","parameters":[{"name":"count","type":"integer","value":"9999999999999999999999"},{"name":"items","type":"array","value":"[\"a\""}]}`)
	f.Add(`{"function":"other","sessionAttributes":{"a":"b"}}`)
	fns := bedrockagent.NewFunctions()
	bedrockagent.Tool(fns, "typed", func(_ context.Context, in typed) (typed, error) { return in, nil })
	f.Fuzz(func(t *testing.T, event string) {
		var e eventsource.BedrockAgentFunctionEvent
		if json.Unmarshal([]byte(event), &e) != nil {
			return
		}
		if _, err := fns.Handle(t.Context(), e); err != nil {
			t.Fatalf("event %s: %v", event, err)
		}
	})
}
