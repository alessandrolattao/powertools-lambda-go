package schemavalidation_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/schemavalidation"
)

func ExampleMiddleware() {
	schema := `{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`
	app := eventhandler.New()
	app.HandleFunc("POST /users", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}, eventhandler.Middlewares(schemavalidation.Middleware(schema, nil)))

	resp, err := eventhandler.APIGatewayREST(app)(context.Background(), events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Path: "/users", Body: `{}`,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.StatusCode, strings.HasPrefix(resp.Body, `{"statusCode":400,"message":"Bad Request: Failed schema validation.`))
	// Output: 400 true
}
