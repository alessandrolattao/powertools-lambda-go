package eventhandler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
)

func Example() {
	app := eventhandler.New()
	app.HandleFunc("GET /hello/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := eventhandler.WriteJSON(w, r, http.StatusOK, map[string]string{"message": "hello " + r.PathValue("name")}); err != nil {
			eventhandler.Error(w, r, err)
		}
	})

	// In main: lambda.Start(eventhandler.APIGatewayREST(app))
	handler := eventhandler.APIGatewayREST(app)
	resp, err := handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: "/hello/lambda"})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.StatusCode, resp.Body)
	// Output: 200 {"message":"hello lambda"}
}

type Todo struct {
	ID    int    `json:"id"`
	Title string `json:"title" required:"true" minLength:"1"`
}

type GetTodo struct {
	ID     int    `path:"id" minimum:"1"`
	Fields string `query:"fields" enum:"all,title" default:"all"`
}

func ExampleGet() {
	app := eventhandler.New()
	eventhandler.Get(app, "/todos/{id}", func(_ context.Context, in GetTodo) (Todo, error) {
		if in.ID == 42 {
			return Todo{}, eventhandler.NotFound("todo 42 not found")
		}
		return Todo{ID: in.ID, Title: "Buy milk"}, nil
	})

	handler := eventhandler.APIGatewayHTTP(app)
	for _, path := range []string{"/todos/1", "/todos/0", "/todos/42"} {
		resp, err := handler(context.Background(), events.APIGatewayV2HTTPRequest{
			RawPath:        path,
			RequestContext: events.APIGatewayV2HTTPRequestContext{HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: "GET"}},
		})
		if err != nil {
			panic(err)
		}
		fmt.Println(resp.StatusCode, resp.Body)
	}
	// Output:
	// 200 {"id":1,"title":"Buy milk"}
	// 422 {"statusCode":422,"detail":[{"loc":["path","id"],"type":"greater_than_equal"}]}
	// 404 {"statusCode":404,"message":"todo 42 not found"}
}

type CreateTodo struct {
	Body Todo
}

func ExamplePost() {
	app := eventhandler.New()
	eventhandler.Post(app, "/todos", func(_ context.Context, in CreateTodo) (eventhandler.Response[Todo], error) {
		in.Body.ID = 7
		return eventhandler.Response[Todo]{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Location": {"/todos/7"}},
			Body:       in.Body,
		}, nil
	}, eventhandler.StatusCode(http.StatusCreated))

	resp, err := eventhandler.APIGatewayREST(app)(context.Background(), events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Path: "/todos", Body: `{"title":"Write docs"}`,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.StatusCode, resp.MultiValueHeaders["Location"], resp.Body)
	// Output: 201 [/todos/7] {"id":7,"title":"Write docs"}
}

var errOutOfStock = errors.New("out of stock")

func ExampleOnError() {
	app := eventhandler.New()
	eventhandler.OnError(app, func(w http.ResponseWriter, r *http.Request, err error) error {
		if errors.Is(err, errOutOfStock) {
			return eventhandler.WriteJSON(w, r, http.StatusConflict, map[string]string{"error": err.Error()})
		}
		return err // the built-in response, or a failed invocation
	})
	eventhandler.Post(app, "/orders", func(context.Context, struct{}) (string, error) {
		return "", fmt.Errorf("order 1: %w", errOutOfStock)
	})

	resp, err := eventhandler.ALB(app)(context.Background(), events.ALBTargetGroupRequest{HTTPMethod: "POST", Path: "/orders"})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.StatusCode, resp.Body)
	// Output: 409 {"error":"order 1: out of stock"}
}

func ExampleRouter_Use() {
	app := eventhandler.New()
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				eventhandler.Error(w, r, eventhandler.Unauthorized("Unauthorized"))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	app.HandleFunc("GET /secret", func(w http.ResponseWriter, r *http.Request) {
		if err := eventhandler.WriteJSON(w, r, http.StatusOK, "s3cr3t"); err != nil {
			eventhandler.Error(w, r, err)
		}
	})

	resp, err := eventhandler.FunctionURL(app)(context.Background(), events.LambdaFunctionURLRequest{
		RawPath:        "/secret",
		RequestContext: events.LambdaFunctionURLRequestContext{HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.StatusCode, resp.Body)
	// Output: 401 {"statusCode":401,"message":"Unauthorized"}
}

func ExampleWithCORS() {
	app := eventhandler.New(eventhandler.WithCORS(eventhandler.CORSConfig{
		AllowOrigin:   "https://www.example.com",
		ExposeHeaders: []string{"X-Request-Id"},
	}))
	app.HandleFunc("GET /todos", func(w http.ResponseWriter, r *http.Request) {
		if err := eventhandler.WriteJSON(w, r, http.StatusOK, []Todo{}); err != nil {
			eventhandler.Error(w, r, err)
		}
	})

	handler := eventhandler.APIGatewayREST(app)
	preflight, err := handler(context.Background(), events.APIGatewayProxyRequest{
		HTTPMethod: "OPTIONS", Path: "/todos",
		Headers: map[string]string{"Origin": "https://www.example.com"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(preflight.StatusCode, preflight.MultiValueHeaders["Access-Control-Allow-Methods"],
		preflight.MultiValueHeaders["Access-Control-Allow-Origin"])
	// Output: 204 [GET,OPTIONS] [https://www.example.com]
}

func ExampleRouter_Include() {
	answer := func(w http.ResponseWriter, r *http.Request) {
		if err := eventhandler.WriteJSON(w, r, http.StatusOK, map[string]string{"id": r.PathValue("id")}); err != nil {
			eventhandler.Error(w, r, err)
		}
	}
	todos := eventhandler.New()
	todos.HandleFunc("GET /", answer)
	todos.HandleFunc("GET /{id}", answer)

	app := eventhandler.New()
	app.Include(todos, "/todos")

	handler := eventhandler.APIGatewayREST(app)
	for _, path := range []string{"/todos", "/todos/3"} {
		resp, err := handler(context.Background(), events.APIGatewayProxyRequest{HTTPMethod: "GET", Path: path})
		if err != nil {
			panic(err)
		}
		fmt.Println(resp.Body)
	}
	// Output:
	// {"id":""}
	// {"id":"3"}
}

func ExampleRouter_OpenAPIJSON() {
	app := eventhandler.New(eventhandler.WithOpenAPI(eventhandler.OpenAPIConfig{Title: "Todos", Version: "1.2.0"}))
	eventhandler.Get(app, "/todos/{id}", func(context.Context, GetTodo) (Todo, error) { return Todo{}, nil },
		eventhandler.Summary("Get a todo"), eventhandler.Tags("todos"))

	doc, err := app.OpenAPIJSON()
	if err != nil {
		panic(err)
	}
	fmt.Println(strings.Contains(string(doc), `"summary": "Get a todo"`), strings.Contains(string(doc), `"$ref": "#/components/schemas/Todo"`))
	// Output: true true
}

func ExampleMergeOpenAPI() {
	orders := eventhandler.New()
	orders.HandleFunc("GET /orders", func(http.ResponseWriter, *http.Request) {})
	users := eventhandler.New()
	users.HandleFunc("GET /users", func(http.ResponseWriter, *http.Request) {})

	ordersDoc, err := orders.OpenAPIJSON()
	if err != nil {
		panic(err)
	}
	usersDoc, err := users.OpenAPIJSON()
	if err != nil {
		panic(err)
	}
	merged, err := eventhandler.MergeOpenAPI(eventhandler.OpenAPIConfig{Title: "Shop"}, eventhandler.ConflictError, ordersDoc, usersDoc)
	if err != nil {
		panic(err)
	}
	fmt.Println(strings.Contains(string(merged), `"/orders"`), strings.Contains(string(merged), `"/users"`))
	// Output: true true
}
