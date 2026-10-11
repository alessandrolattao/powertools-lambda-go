package logging

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectContext_AnEventTheCorrelationPathCannotReadDoesNotFail(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(WithWriter(&buf))
	h := l.InjectContext(func(ctx context.Context, _ jsontext.Value) (string, error) {
		l.InfoContext(ctx, "handled")
		return "ok", nil
	}, WithCorrelationIDPath(CorrelationAPIGatewayHTTP))

	got, err := h(t.Context(), jsontext.Value("\xff not JSON"))
	require.NoError(t, err, "a logging feature never stops the function")
	assert.Equal(t, "ok", got)
	assert.Contains(t, buf.String(), `"message":"handled"`)
	assert.NotContains(t, buf.String(), "correlation_id")
}

func TestInjectContext_DuplicateNamesKeepTheLast(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := New(WithWriter(&buf))
	h := l.InjectContext(func(ctx context.Context, _ jsontext.Value) (string, error) {
		l.InfoContext(ctx, "handled")
		return "ok", nil
	}, WithCorrelationIDPath(CorrelationAPIGatewayHTTP))

	_, err := h(t.Context(), jsontext.Value(`{"requestContext":{"requestId":"a","requestId":"b"}}`))
	require.NoError(t, err)
	assert.Contains(t, buf.String(), `"correlation_id":"b"`, "as Python's json module reads it")
}
