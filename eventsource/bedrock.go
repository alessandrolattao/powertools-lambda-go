package eventsource

// BedrockAgentEvent is what an Amazon Bedrock agent sends to the Lambda
// function of an action group defined with an OpenAPI schema.
type BedrockAgentEvent struct {
	MessageVersion          string                   `json:"messageVersion"`
	InputText               string                   `json:"inputText"`
	SessionID               string                   `json:"sessionId"`
	ActionGroup             string                   `json:"actionGroup"`
	APIPath                 string                   `json:"apiPath"`
	HTTPMethod              string                   `json:"httpMethod"`
	Parameters              []BedrockAgentProperty   `json:"parameters"`
	RequestBody             *BedrockAgentRequestBody `json:"requestBody,omitempty"`
	Agent                   BedrockAgentInfo         `json:"agent"`
	SessionAttributes       map[string]string        `json:"sessionAttributes"`
	PromptSessionAttributes map[string]string        `json:"promptSessionAttributes"`
}

// BedrockAgentInfo identifies the agent.
type BedrockAgentInfo struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Alias   string `json:"alias"`
	Version string `json:"version"`
}

// BedrockAgentProperty is a parameter or a body property the agent
// extracted from the conversation. Values are always strings; Type says
// what they hold (string, number, integer, boolean, array).
type BedrockAgentProperty struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// BedrockAgentRequestBody is the request body, by media type.
type BedrockAgentRequestBody struct {
	Content map[string]BedrockAgentMediaType `json:"content"`
}

// BedrockAgentMediaType holds the properties of the body for one media type.
type BedrockAgentMediaType struct {
	Properties []BedrockAgentProperty `json:"properties"`
}

// QueryParameters returns the parameters by name, Powertools Python's
// query_string_parameters for Bedrock agents.
func (e BedrockAgentEvent) QueryParameters() map[string]string {
	params := make(map[string]string, len(e.Parameters))
	for _, p := range e.Parameters {
		params[p.Name] = p.Value
	}
	return params
}

// BodyProperties returns the application/json body properties by name, the
// equivalent of Powertools Python's json_body for Bedrock agents. It
// reports false when the request has no JSON body.
func (e BedrockAgentEvent) BodyProperties() (map[string]string, bool) {
	if e.RequestBody == nil {
		return nil, false
	}
	media, ok := e.RequestBody.Content["application/json"]
	if !ok {
		return nil, false
	}
	props := make(map[string]string, len(media.Properties))
	for _, p := range media.Properties {
		props[p.Name] = p.Value
	}
	return props, true
}

// BedrockAgentFunctionEvent is what an Amazon Bedrock agent sends to the
// Lambda function of an action group defined with function details.
type BedrockAgentFunctionEvent struct {
	MessageVersion          string                 `json:"messageVersion"`
	InputText               string                 `json:"inputText"`
	SessionID               string                 `json:"sessionId"`
	ActionGroup             string                 `json:"actionGroup"`
	Function                string                 `json:"function"`
	Parameters              []BedrockAgentProperty `json:"parameters"`
	Agent                   BedrockAgentInfo       `json:"agent"`
	SessionAttributes       map[string]string      `json:"sessionAttributes"`
	PromptSessionAttributes map[string]string      `json:"promptSessionAttributes"`
}
