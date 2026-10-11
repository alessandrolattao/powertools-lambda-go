package eventhandler

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// do serves one request with app and returns the recorded response.
func do(t *testing.T, app http.Handler, method, target, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

// text answers with the given text.
func text(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentTypeText)
		if _, err := io.WriteString(w, s); err != nil {
			panic(err)
		}
	})
}

// write writes s, failing the test server loudly when it cannot.
func write(w io.Writer, s string) {
	if _, err := io.WriteString(w, s); err != nil {
		panic(err)
	}
}

// pythonBody checks that a response body is want byte for byte: the compact
// JSON Powertools Python writes, members in its order.
func pythonBody(t *testing.T, want, got string, context ...any) {
	t.Helper()
	if got != want {
		t.Errorf("%v\n got: %s\nwant: %s", context, got, want)
	}
}

// loadEvent decodes a sample event of Powertools Python's test suite.
func loadEvent[E any](t *testing.T, name string) E {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "events", name))
	require.NoError(t, err)
	var e E
	require.NoError(t, json.Unmarshal(data, &e), name)
	return e
}

// str returns v as a string, failing the test when it is not one.
func str(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	require.True(t, ok, "%v is not a string", v)
	return s
}

// decode decodes a JSON body.
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &m), body)
	return m
}
