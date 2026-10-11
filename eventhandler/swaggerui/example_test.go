package swaggerui_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/swaggerui"
)

func ExampleEnable() {
	app := eventhandler.New(eventhandler.WithOpenAPI(eventhandler.OpenAPIConfig{Title: "Todos"}))
	eventhandler.Get(app, "/todos", func(context.Context, struct{}) ([]string, error) { return nil, nil })
	swaggerui.Enable(app, swaggerui.WithCompression())

	handler := eventhandler.APIGatewayREST(app)
	page, err := handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/swagger"})
	if err != nil {
		panic(err)
	}
	doc, err := handler(context.Background(), events.APIGatewayProxyRequest{
		HTTPMethod: "GET", Path: "/swagger", QueryStringParameters: map[string]string{"format": "json"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(page.StatusCode, page.MultiValueHeaders["Content-Type"])
	fmt.Println(strings.Contains(doc.Body, `"title": "Todos"`))
	// Output:
	// 200 [text/html]
	// true
}
