package lambdahttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseBuffer(t *testing.T) {
	t.Parallel()
	b := NewResponseBuffer()
	assert.Equal(t, http.StatusOK, b.StatusCode(), "200 until written")
	b.Header().Set("X-Kept", "1")
	b.Header().Set("Content-Type", "text/plain")
	b.WriteHeader(http.StatusCreated)
	b.WriteHeader(http.StatusTeapot)
	n, err := b.Write([]byte("partial"))
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	assert.Equal(t, http.StatusCreated, b.StatusCode(), "the first status wins")

	b.Reset()
	assert.Empty(t, b.Body())
	assert.Equal(t, "1", b.Header().Get("X-Kept"), "headers set before stay")
	assert.Empty(t, b.Header().Get("Content-Type"), "headers describing the body go")
	b.SetStatus(http.StatusAccepted)
	b.SetBody([]byte("final"))

	rec := httptest.NewRecorder()
	require.NoError(t, b.CopyTo(rec))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "final", rec.Body.String())
}

func TestServe(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	inv := &Invocation{}
	req := httptest.NewRequestWithContext(NewContext(t.Context(), inv), http.MethodGet, "/", nil)
	buf, err := Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		FromContext(r.Context()).Err = boom
		if _, werr := w.Write([]byte("<html></html>")); werr != nil {
			panic(werr)
		}
	}), req, inv)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, "text/html; charset=utf-8", buf.Header().Get("Content-Type"), "sniffed as net/http does")
	assert.Nil(t, FromContext(t.Context()))
}
