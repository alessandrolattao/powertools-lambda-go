package integration

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/swaggerui"
)

type todo struct {
	ID    int    `json:"id"`
	Title string `json:"title" required:"true" minLength:"1"`
}

// eventHandlerApp is a router served by a real HTTP server, the local
// development setup of the event handler.
func eventHandlerApp() *eventhandler.Router {
	app := eventhandler.New(eventhandler.WithCORS(eventhandler.CORSConfig{AllowOrigin: "https://app.example.com"}))
	eventhandler.Get(app, "/todos/{id}", func(_ context.Context, in struct {
		ID int `path:"id" minimum:"1"`
	},
	) (todo, error) {
		return todo{ID: in.ID, Title: strings.Repeat("buy milk ", 50)}, nil
	}, eventhandler.Compress())
	eventhandler.Post(app, "/todos", func(_ context.Context, in struct{ Body todo }) (eventhandler.Response[todo], error) {
		return eventhandler.Response[todo]{StatusCode: http.StatusCreated, Body: in.Body}, nil
	})
	eventhandler.Post(app, "/upload", func(_ context.Context, in struct {
		File *multipart.FileHeader `formData:"file" required:"true"`
	},
	) (int64, error) {
		return in.File.Size, nil
	})
	swaggerui.Enable(app)
	return app
}

func TestEventHandlerOverHTTP(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(eventHandlerApp())
	t.Cleanup(server.Close)
	client := server.Client()
	// reply is what the server answered.
	type reply struct {
		status       int
		header       http.Header
		uncompressed bool
	}
	send := func(method, path, contentType string, body io.Reader, header ...string) (reply, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, body)
		require.NoError(t, err)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return reply{status: resp.StatusCode, header: resp.Header, uncompressed: resp.Uncompressed}, string(data)
	}

	resp, body := send(http.MethodGet, "/todos/7", "", nil)
	assert.Equal(t, http.StatusOK, resp.status)
	assert.True(t, resp.uncompressed, "the client asked for gzip and got it")
	assert.Contains(t, body, `"id":7`)

	resp, body = send(http.MethodGet, "/todos/0", "", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.status)
	assert.JSONEq(t, `{"statusCode":422,"detail":[{"loc":["path","id"],"type":"greater_than_equal"}]}`, body)

	resp, body = send(http.MethodPost, "/todos", "application/json", strings.NewReader(`{"id":1,"title":"x"}`))
	assert.Equal(t, http.StatusCreated, resp.status)
	assert.JSONEq(t, `{"id":1,"title":"x"}`, body)

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, err := mw.CreateFormFile("file", "a.bin")
	require.NoError(t, err)
	_, err = fw.Write(make([]byte, 1000))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	resp, body = send(http.MethodPost, "/upload", mw.FormDataContentType(), &form)
	assert.Equal(t, http.StatusOK, resp.status)
	assert.Equal(t, "1000", body)

	resp, _ = send(http.MethodOptions, "/todos", "", nil, "Origin", "https://app.example.com")
	assert.Equal(t, http.StatusNoContent, resp.status)
	assert.Equal(t, "GET,OPTIONS,POST", resp.header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "https://app.example.com", resp.header.Get("Access-Control-Allow-Origin"))

	resp, body = send(http.MethodGet, "/swagger?format=json", "", nil)
	assert.Equal(t, http.StatusOK, resp.status)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &doc))
	assert.Equal(t, "3.1.0", doc["openapi"])
	resp, body = send(http.MethodGet, "/swagger", "", nil)
	assert.Equal(t, "text/html", resp.header.Get("Content-Type"))
	assert.Contains(t, body[:200], "<title>Swagger UI</title>")

	resp, _ = send(http.MethodHead, "/todos/7", "", nil)
	assert.Equal(t, http.StatusNotFound, resp.status, "GET does not answer HEAD")
}
