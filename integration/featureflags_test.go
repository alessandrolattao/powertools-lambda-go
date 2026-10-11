package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/featureflags"
)

const ffDocument = `{
	"log_level": "INFO",
	"features": {
		"premium_features": {
			"default": false,
			"rules": {
				"premium customers": {"when_match": true, "conditions": [{"action": "EQUALS", "key": "tier", "value": "premium"}]},
				"also premium, never reached": {"when_match": false, "conditions": [{"action": "EQUALS", "key": "tier", "value": "premium"}]}
			}
		},
		"canary": {
			"default": false,
			"rules": {"ten percent": {"when_match": true, "conditions": [{"action": "MODULO_RANGE", "key": "user_id", "value": {"BASE": 10, "START": 0, "END": 0}}]}}
		},
		"limits": {"default": {"requests": 10}, "boolean_type": false, "rules": {
			"enterprise": {"when_match": {"requests": 1000}, "conditions": [{"action": "KEY_IN_VALUE", "key": "tier", "value": ["enterprise", "partner"]}]}
		}},
		"business_hours": {
			"default": false,
			"rules": {"office": {"when_match": true, "conditions": [{"action": "SCHEDULE_BETWEEN_TIME_RANGE", "key": "CURRENT_TIME", "value": {"START": "09:00", "END": "17:00", "TIMEZONE": "Europe/Rome"}}]}}
		}
	}
}`

func TestFeatureFlagsOnAppConfig(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	application, environment, profile := paramsDeployAppConfig(t, ffDocument)
	store := featureflags.AppConfigStoreFromConfig(suite.aws, application, environment, profile, featureflags.WithEnvelope("features"))
	tenInRome := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	flags := featureflags.New(store, featureflags.WithClock(func() time.Time { return tenInRome }))

	premium, err := flags.Evaluate(ctx, "premium_features", map[string]any{"tier": "premium"}, false)
	require.NoError(t, err)
	assert.True(t, premium, "rules apply in document order")

	canary, err := flags.Evaluate(ctx, "canary", map[string]any{"user_id": 1230}, false)
	require.NoError(t, err)
	assert.True(t, canary)

	type limits struct {
		Requests int `json:"requests"`
	}
	enterprise, err := flags.Evaluate(ctx, "limits", map[string]any{"tier": "partner"}, limits{})
	require.NoError(t, err)
	assert.Equal(t, 1000, enterprise.Requests)

	enabled, err := flags.EnabledFeatures(ctx, map[string]any{"tier": "premium", "user_id": 7})
	require.NoError(t, err)
	assert.Equal(t, []string{"premium_features", "limits", "business_hours"}, enabled)

	missing, err := flags.Evaluate(ctx, "no_such_feature", nil, true)
	require.NoError(t, err)
	assert.True(t, missing)

	raw, err := store.GetRawConfiguration(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "log_level")
}

func TestFeatureFlagsUnknownProfile(t *testing.T) {
	t.Parallel()
	application, environment, _ := paramsDeployAppConfig(t, `{}`)
	flags := featureflags.New(featureflags.AppConfigStoreFromConfig(suite.aws, application, environment, "no-such-profile"))
	got, err := flags.Evaluate(t.Context(), "anything", nil, true)
	require.NoError(t, err, "a store that cannot provide the schema gives the default")
	assert.True(t, got)
}
