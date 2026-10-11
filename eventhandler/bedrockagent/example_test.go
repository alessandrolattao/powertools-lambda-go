package bedrockagent_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/bedrockagent"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

type WeatherInput struct {
	City string `query:"city" required:"true" description:"The city to look up"`
}

type Weather struct {
	City        string  `json:"city"`
	Temperature float64 `json:"temperature"`
}

func ExampleHandler() {
	app := eventhandler.New()
	eventhandler.Get(app, "/weather", func(ctx context.Context, in WeatherInput) (Weather, error) {
		bedrockagent.SetSessionAttributes(ctx, map[string]string{"lastCity": in.City})
		return Weather{City: in.City, Temperature: 21.5}, nil
	}, eventhandler.Description("Current weather of a city"))

	// In main: lambda.Start(bedrockagent.Handler(app))
	resp, err := bedrockagent.Handler(app)(context.Background(), eventsource.BedrockAgentEvent{
		ActionGroup: "weather", HTTPMethod: "GET", APIPath: "/weather",
		Parameters: []eventsource.BedrockAgentProperty{{Name: "city", Type: "string", Value: "Rome"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.Response.HTTPStatusCode, resp.Response.ResponseBody["application/json"].Body, resp.SessionAttributes)
	// Output: 200 {"city":"Rome","temperature":21.5} map[lastCity:Rome]
}

func ExampleOpenAPIJSON() {
	app := eventhandler.New()
	eventhandler.Get(app, "/weather", func(context.Context, WeatherInput) (Weather, error) { return Weather{}, nil },
		eventhandler.Description("Current weather of a city"), bedrockagent.RequireConfirmation())

	doc, err := bedrockagent.OpenAPIJSON(app)
	if err != nil {
		panic(err)
	}
	fmt.Println(strings.Contains(string(doc), `"openapi": "3.0.3"`), strings.Contains(string(doc), `"x-requireConfirmation": "ENABLED"`))
	// Output: true true
}

func ExampleTool() {
	fns := bedrockagent.NewFunctions()
	bedrockagent.Tool(fns, "get_weather", func(_ context.Context, in struct {
		City string `json:"city"`
		Days int    `json:"days"`
	},
	) (string, error) {
		return fmt.Sprintf("Sunny in %s for %d days", in.City, in.Days), nil
	})

	// In main: lambda.Start(fns.Handle)
	resp, err := fns.Handle(context.Background(), eventsource.BedrockAgentFunctionEvent{
		ActionGroup: "weather", Function: "get_weather",
		Parameters: []eventsource.BedrockAgentProperty{
			{Name: "city", Type: "string", Value: "Rome"},
			{Name: "days", Type: "integer", Value: "3"},
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(resp.Response.FunctionResponse.ResponseBody["TEXT"].Body)
	// Output: Sunny in Rome for 3 days
}
