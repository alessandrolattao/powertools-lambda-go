package validation_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"regexp"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/validation"
)

// userSchema is compiled once, when the function initializes.
var userSchema = validation.MustCompile(`{
	"type": "object",
	"required": ["user_id", "project"],
	"properties": {
		"user_id": {"type": "string", "format": "uuid"},
		"project": {"type": "string", "enum": ["powertools", "other"]},
		"ip": {"type": "string", "format": "ipv4"},
		"role": {"type": "string", "default": "reader"}
	}
}`)

func ExampleValidate() {
	event := []byte(`{"user_id":"0d44b083-8206-4a3a-aa95-5d392a99be4a","project":"lambda"}`)

	_, err := validation.Validate(event, userSchema)
	if verr, ok := errors.AsType[*validation.SchemaValidationError](err); ok {
		fmt.Println(verr.Rule, verr.Path)
		fmt.Println(verr)
	}
	// Output:
	// enum [data project]
	// Failed schema validation. Error: data.project must be one of ['powertools', 'other'], Path: ['data', 'project'], Data: lambda
}

func ExampleValidate_envelope() {
	event := events.EventBridgeEvent{
		DetailType: "UserCreated",
		Source:     "my.app",
		Detail:     jsontext.Value(`{"user_id":"0d44b083-8206-4a3a-aa95-5d392a99be4a","project":"powertools"}`),
	}

	detail, err := validation.Validate(event, userSchema, validation.WithEnvelope(validation.EventBridge))
	if err != nil {
		panic(err)
	}
	fields, ok := detail.(map[string]any)
	if !ok {
		panic("not an object")
	}
	fmt.Println(fields["role"])
	// Output: reader
}

func ExampleValidateAs() {
	type user struct {
		UserID  string `json:"user_id"`
		Project string `json:"project"`
		Role    string `json:"role"`
	}

	u, err := validation.ValidateAs[user](
		[]byte(`{"user_id":"0d44b083-8206-4a3a-aa95-5d392a99be4a","project":"powertools"}`), userSchema)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%+v\n", u)
	// Output: {UserID:0d44b083-8206-4a3a-aa95-5d392a99be4a Project:powertools Role:reader}
}

func ExampleValidator() {
	type order struct {
		ID    string  `json:"id"`
		Total float64 `json:"total"`
	}
	type receipt struct {
		OrderID string `json:"orderId"`
		Status  string `json:"status"`
	}

	handle := func(_ context.Context, o order) (receipt, error) {
		return receipt{OrderID: o.ID, Status: "accepted"}, nil
	}

	// lambda.Start(h) in a function; called directly here.
	h := validation.Validator(handle,
		validation.Inbound(`{"type":"object","required":["id","total"],"properties":{"total":{"minimum":0}}}`,
			validation.WithEnvelope(validation.APIGatewayREST)),
		validation.Outbound(`{"type":"object","required":["orderId","status"]}`),
	)

	ok, err := h(context.Background(), jsontext.Value(`{"body":"{\"id\":\"o-1\",\"total\":12.5}"}`))
	fmt.Println(ok, err)

	_, err = h(context.Background(), jsontext.Value(`{"body":"{\"id\":\"o-2\",\"total\":-1}"}`))
	fmt.Println(err)
	// Output:
	// {o-1 accepted} <nil>
	// Failed schema validation. Error: data.total must be bigger than or equal to 0, Path: ['data', 'total'], Data: -1
}

func ExampleWithFormat() {
	accountID := regexp.MustCompile(`^\d{12}$`)
	schema := validation.MustCompile(`{"properties":{"accountid":{"type":"string","format":"awsaccountid"}}}`,
		validation.WithFormat("awsaccountid", accountID.MatchString))

	_, err := schema.Validate([]byte(`{"accountid":"200984112386"}`))
	fmt.Println(err)
	_, err = schema.Validate([]byte(`{"accountid":"1234"}`))
	fmt.Println(err)
	// Output:
	// <nil>
	// Failed schema validation. Error: data.accountid must be awsaccountid, Path: ['data', 'accountid'], Data: 1234
}

func ExampleWithLoader() {
	const child = `{"type":"object","properties":{"project":{"type":"string"}},"required":["project"]}`
	parent := `{"type":"object","properties":{"ParentSchema":{"$ref":"https://example.com/schemas/child.json"}}}`

	schema, err := validation.Compile(parent, validation.WithLoader("https", func(context.Context, string) (any, error) {
		// Fetch from S3, a parameter, the file system: here, a constant.
		return child, nil
	}))
	if err != nil {
		panic(err)
	}
	_, err = schema.Validate([]byte(`{"ParentSchema":{}}`))
	fmt.Println(err)
	// Output: Failed schema validation. Error: data.ParentSchema must contain ['project'] properties, Path: ['data', 'ParentSchema'], Data: {}
}
