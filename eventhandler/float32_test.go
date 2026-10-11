package eventhandler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
)

func TestFloat32ParametersValidateAtTheirOwnPrecision(t *testing.T) {
	t.Parallel()
	type input struct {
		Ratio float32 `query:"ratio" maximum:"0.1" required:"true"`
	}
	r := eventhandler.New()
	eventhandler.Get(r, "/ratio", func(_ context.Context, in input) (float32, error) { return in.Ratio, nil })

	for raw, status := range map[string]int{"0.1": http.StatusOK, "0.09": http.StatusOK, "0.11": http.StatusUnprocessableEntity} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ratio?ratio="+raw, http.NoBody))
		assert.Equal(t, status, rec.Code, "ratio=%s: %s", raw, rec.Body.String())
	}
}
