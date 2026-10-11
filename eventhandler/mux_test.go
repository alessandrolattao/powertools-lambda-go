package eventhandler

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMountedUnderServeMux(t *testing.T) {
	t.Parallel()
	app := New()
	app.HandleFunc("GET /api/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		write(w, r.PathValue("id")+"|"+r.PathValue("tenant"))
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/{tenant}/api/", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/"+r.PathValue("tenant"), app).ServeHTTP(w, r)
		write(w, "|outer:"+r.PathValue("id"))
	})
	rec := do(t, mux, "GET", "/acme/api/7/items", "")
	assert.Equal(t, "7|acme|outer:", rec.Body.String(), "inner values do not leak into the outer request")
}
