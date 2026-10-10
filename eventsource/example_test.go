package eventsource_test

import (
	"encoding/json/v2"
	"fmt"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

func ExampleHeader() {
	event := events.APIGatewayProxyRequest{
		Headers: map[string]string{"Content-Type": "application/json", "Cookie": "session=abc; theme=dark"},
		Body:    `{"id":"o-1"}`,
	}

	contentType, _ := eventsource.Header(event, "content-type")
	fmt.Println(contentType)
	fmt.Println(eventsource.Cookies(event)["session"])
	// Output:
	// application/json
	// abc
}

func ExampleJSONBody() {
	type order struct {
		ID string `json:"id"`
	}
	event := events.APIGatewayV2HTTPRequest{Body: "eyJpZCI6Im8tMSJ9", IsBase64Encoded: true}

	o, err := eventsource.JSONBody[order](event)
	if err != nil {
		panic(err)
	}
	fmt.Println(o.ID)
	// Output: o-1
}

func ExampleNewAuthorizerResponse() {
	arn, err := eventsource.ParseRouteARN("arn:aws:execute-api:us-east-1:123456789012:api1/prod/GET/orders")
	if err != nil {
		panic(err)
	}

	response := eventsource.NewAuthorizerResponse("user-1", arn)
	if routeErr := response.AllowRoute(eventsource.HTTPVerbGet, "/orders/*"); routeErr != nil {
		panic(routeErr)
	}
	policy, err := response.Build()
	if err != nil {
		panic(err)
	}
	fmt.Println(policy.PolicyDocument.Statement[0].Effect, policy.PolicyDocument.Statement[0].Resource[0])
	// Output: Allow arn:aws:execute-api:us-east-1:123456789012:api1/prod/GET/orders/*
}

func ExampleDecodeDynamoDBImage() {
	type item struct {
		ID    string  `json:"pk"`
		Total float64 `json:"total"`
	}
	image := map[string]events.DynamoDBAttributeValue{
		"pk":    events.NewStringAttribute("order#1"),
		"total": events.NewNumberAttribute("12.50"),
	}

	got, err := eventsource.DecodeDynamoDBImage[item](image)
	if err != nil {
		panic(err)
	}
	fmt.Println(got.ID, got.Total)
	// Output: order#1 12.5
}

func ExampleConfigEvent_DecodeInvokingEvent() {
	event := eventsource.ConfigEvent{
		InvokingEvent: `{"messageType":"ScheduledNotification","awsAccountId":"123456789012"}`,
	}

	invoking, err := event.DecodeInvokingEvent()
	if err != nil {
		panic(err)
	}
	switch e := invoking.(type) {
	case *eventsource.ConfigScheduled:
		fmt.Println("scheduled evaluation of", e.AWSAccountID)
	case *eventsource.ConfigConfigurationChanged:
		fmt.Println("change of", e.ConfigurationItem.ResourceID)
	case *eventsource.ConfigOversizedConfiguration:
		fmt.Println("oversized change of", e.ConfigurationItemSummary.ResourceID)
	}
	// Output: scheduled evaluation of 123456789012
}

func ExampleVPCLatticeEventV2() {
	raw := []byte(`{"version":"2.0","path":"/orders","method":"GET",
		"headers":{"accept":["application/json"]},"queryStringParameters":{"id":["1","2"]},
		"requestContext":{"timeEpoch":"1696331543569073"}}`)

	var event eventsource.VPCLatticeEventV2
	if err := json.Unmarshal(raw, &event); err != nil {
		panic(err)
	}
	fmt.Println(eventsource.Path(event), eventsource.QueryParameterValues(event, "id"))
	fmt.Println(event.RequestContext.TimeEpoch.Time().Format("2006-01-02"))
	// Output:
	// /orders [1 2]
	// 2023-10-03
}

func ExampleNestedSNSMessage() {
	message := events.SQSMessage{
		Body: `{"Type":"Notification","Message":"hello","UnsubscribeURL":"https://sns.example.com/unsubscribe"}`,
	}

	notification, err := eventsource.NestedSNSMessage(message)
	if err != nil {
		panic(err)
	}
	fmt.Println(notification.Message, notification.UnsubscribeURL)
	// Output: hello https://sns.example.com/unsubscribe
}

func ExampleNewTransferFamilyS3Response() {
	response, err := eventsource.NewTransferFamilyS3Response("arn:aws:iam::123456789012:role/sftp-user",
		eventsource.WithHomeDirectory("/my-bucket/home/alice"))
	if err != nil {
		panic(err)
	}
	out, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
	// Output: {"Role":"arn:aws:iam::123456789012:role/sftp-user","HomeDirectoryType":"PATH","HomeDirectory":"/my-bucket/home/alice"}
}
