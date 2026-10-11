// Command openapimerge writes the OpenAPI document of an API whose routes
// live in two Lambda functions, the Go side of Powertools Python's
// OpenAPIMerge. Each function package exports the function that registers
// its routes; a build step imports them all, renders each document and merges
// them:
//
//	go run ./eventhandler/examples/openapimerge openapi.json
//
// The functions here stand for those packages.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
)

type Order struct {
	ID    string `json:"id" required:"true"`
	Total int    `json:"total" minimum:"0"`
}

type User struct {
	ID   string `json:"id" required:"true"`
	Name string `json:"name"`
}

type byID struct {
	ID string `path:"id"`
}

// registerOrders is what the orders function's package would export.
func registerOrders(app *eventhandler.Router) {
	eventhandler.Get(app, "/orders/{id}", func(_ context.Context, in byID) (Order, error) {
		return Order{ID: in.ID}, nil
	}, eventhandler.Tags("orders"), eventhandler.Summary("Get an order"))
}

// registerUsers is what the users function's package would export.
func registerUsers(app *eventhandler.Router) {
	eventhandler.Get(app, "/users/{id}", func(_ context.Context, in byID) (User, error) {
		return User{ID: in.ID}, nil
	}, eventhandler.Tags("users"), eventhandler.Summary("Get a user"))
}

func main() {
	out := "openapi.json"
	if len(os.Args) > 1 {
		out = filepath.Clean(os.Args[1])
	}
	if err := run(out); err != nil {
		slog.Error("openapimerge failed", "err", err)
		os.Exit(1)
	}
}

func run(out string) error {
	registrations := []func(*eventhandler.Router){registerOrders, registerUsers}
	docs := make([][]byte, 0, len(registrations))
	for _, register := range registrations {
		app := eventhandler.New()
		register(app)
		doc, err := app.OpenAPIJSON()
		if err != nil {
			return err
		}
		docs = append(docs, doc)
	}
	merged, err := eventhandler.MergeOpenAPI(eventhandler.OpenAPIConfig{
		Title:   "Shop API",
		Version: "1.0.0",
		Servers: []eventhandler.Server{{URL: "https://api.example.com"}},
	}, eventhandler.ConflictError, docs...)
	if err != nil {
		return err
	}
	// G703: the output path is the command line argument of the person
	// running the build step.
	if err := os.WriteFile(out, append(merged, '\n'), 0o600); err != nil { //nolint:gosec // see above
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}
