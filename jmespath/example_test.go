package jmespath_test

import (
	"fmt"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/jmespath"
)

func ExampleQuery() {
	event := events.APIGatewayV2HTTPRequest{Body: `{"customerId":"c-42"}`}

	payload, err := jmespath.Query(event, jmespath.APIGatewayHTTP+".customerId")
	if err != nil {
		panic(err)
	}
	fmt.Println(payload)
	// Output: c-42
}

func ExampleQueryAs() {
	type order struct {
		ID string `json:"id"`
	}
	event := events.SQSEvent{Records: []events.SQSMessage{{Body: `{"id":"o-1"}`}, {Body: `{"id":"o-2"}`}}}

	orders, err := jmespath.QueryAs[[]order](event, jmespath.SQS)
	if err != nil {
		panic(err)
	}
	fmt.Println(orders)
	// Output: [{o-1} {o-2}]
}

func ExampleCompile() {
	// Compile once, search on every invocation.
	userID := jmespath.MustCompile("requestContext.authorizer.jwt.claims.sub")

	event := events.APIGatewayV2HTTPRequest{RequestContext: events.APIGatewayV2HTTPRequestContext{
		Authorizer: &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
			JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{
				Claims: map[string]string{"sub": "user-7"},
			},
		},
	}}
	sub, err := userID.Search(event)
	if err != nil {
		panic(err)
	}
	fmt.Println(sub)
	// Output: user-7
}
