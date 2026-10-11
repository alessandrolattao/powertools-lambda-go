package oauth2_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/alessandrolattao/powertools-lambda-go/oauth2"
)

// provider plays the token endpoint and the downstream API of the
// examples, which share a test certificate the returned client trusts.
func provider() (tokenURL, apiURL string, trusting *http.Client, stop func()) {
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"access_token":"tok-1","token_type":"Bearer","expires_in":3600}`); err != nil {
			panic(err)
		}
	}))
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if _, err := io.WriteString(w, "stock for "+r.Header.Get("Authorization")); err != nil { //nolint:gosec // G705: a test server echoing to its own client
			panic(err)
		}
	}))
	return idp.URL + "/oauth2/token", api.URL, idp.Client(), func() { idp.Close(); api.Close() }
}

func ExampleNew() {
	tokenURL, _, trusting, stop := provider()
	defer stop()

	// Build the client once, outside the handler, so that warm invocations
	// reuse its token. In a function, the secret source typically reads
	// Secrets Manager through the parameters package.
	client, err := oauth2.New(tokenURL, "orders", oauth2.StaticSecret("s3cret"),
		oauth2.WithScopes("inventory:read"),
		oauth2.WithResource("https://inventory.example.com"),
		oauth2.WithHTTPClient(trusting),
	)
	if err != nil {
		panic(err)
	}

	header, err := client.AuthHeaders(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(header.Get("Authorization"))
	// Output: Bearer tok-1
}

func ExampleClient_HTTPClient() {
	tokenURL, apiURL, trusting, stop := provider()
	defer stop()
	client, err := oauth2.New(tokenURL, "orders", oauth2.StaticSecret("s3cret"), oauth2.WithHTTPClient(trusting))
	if err != nil {
		panic(err)
	}

	inventory := client.HTTPClient()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, apiURL+"/stock/item-1", http.NoBody)
	if err != nil {
		panic(err)
	}
	resp, err := inventory.Do(req)
	if err != nil {
		panic(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	if err := resp.Body.Close(); err != nil {
		panic(err)
	}
	fmt.Println(resp.StatusCode, string(body))
	// Output: 200 stock for Bearer tok-1
}
