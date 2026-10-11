package oauth2

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// FuzzTokenResponse feeds arbitrary token endpoint answers to an exchange
// attempt: whatever the provider sends, the client returns a well-formed
// token or an error, never panics.
func FuzzTokenResponse(f *testing.F) {
	f.Add(200, "application/json", `{"access_token":"a","token_type":"Bearer","expires_in":3600}`)
	f.Add(200, "application/json", `{"access_token":"a b","token_type":"mac","expires_in":"x"}`)
	f.Add(200, "application/x-www-form-urlencoded", `access_token=a&token_type=bearer&expires_in=1`)
	f.Add(400, "application/json", `{"error":"invalid_client","error_description":"s3cret"}`)
	f.Add(503, "", ``)
	f.Add(200, "application/json", `{"access_token":"a","token_type":"Bearer","expires_in":1e400}`)
	f.Fuzz(func(t *testing.T, status int, contentType, body string) {
		if status < 100 || status > 999 {
			status = http.StatusOK
		}
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": {contentType}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})
		c, err := New("https://idp.example.com/token", "orders", StaticSecret("s3cret"), WithHTTPClient(&http.Client{Transport: base}))
		if err != nil {
			t.Fatal(err)
		}
		token, _, err := c.attempt(t.Context())
		if err != nil {
			return
		}
		if !bearerToken.MatchString(token.value) {
			t.Fatalf("not a bearer token: %q", token.value)
		}
	})
}
