package eventsource

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppSyncIdentityThatIsNotAnObject(t *testing.T) {
	t.Parallel()
	var event AppSyncResolverEvent
	require.NoError(t, json.Unmarshal([]byte(`{"identity":"None","info":{"fieldName":"getTodo","parentTypeName":"Query"}}`), &event))
	require.NotNil(t, event.Identity)
	assert.Equal(t, AppSyncIdentity{}, *event.Identity)

	require.NoError(t, json.Unmarshal([]byte(`{"identity":{"sub":"u-1","issuer":"https://cognito","username":"ada","defaultAuthStrategy":"ALLOW"}}`), &event))
	assert.Equal(t, "u-1", event.Identity.Sub)
	assert.Equal(t, "ada", event.Identity.Username)
	assert.Equal(t, AppSyncAuthorizationCognito, event.Identity.Authorization())
}
