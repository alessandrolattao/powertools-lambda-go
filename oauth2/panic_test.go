package oauth2

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errVaultDown = errors.New("vault down")

// A secret source that panics once must not leave the client waiting on an
// exchange that will never end.
func TestAPanickingSecretSourceDoesNotWedgeTheClient(t *testing.T) {
	t.Parallel()
	e := newTokenEndpoint(t, tokenReply("tok-1", "3600"))
	var calls atomic.Int32
	source := func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			panic(errVaultDown)
		}
		return "s3cret", nil
	}
	c, err := New(e.url(), "orders", source, WithHTTPClient(e.server.Client()), WithTimeout(2*time.Second))
	require.NoError(t, err)

	assert.PanicsWithValue(t, errVaultDown, func() {
		_, _ = c.Token(t.Context()) //nolint:errcheck // it panics
	})

	start := time.Now()
	token, err := c.Token(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Less(t, time.Since(start), time.Second, "the next caller does not wait for the timeout")
}
