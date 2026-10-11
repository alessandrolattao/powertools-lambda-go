package featureflags_test

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/featureflags"
)

// memoryStore is a custom store: a fixed document. A real one would read S3,
// a database, a file.
type memoryStore jsontext.Value

func (s memoryStore) GetConfiguration(context.Context) (jsontext.Value, error) {
	return jsontext.Value(s), nil
}

const schema = `{
	"premium_features": {
		"default": false,
		"rules": {
			"customer tier equals premium": {
				"when_match": true,
				"conditions": [{"action": "EQUALS", "key": "tier", "value": "premium"}]
			}
		}
	},
	"ten_percent_off_campaign": {"default": true},
	"discount": {
		"default": {"percent": 0},
		"boolean_type": false,
		"rules": {
			"loyal customers": {
				"when_match": {"percent": 15},
				"conditions": [{"action": "KEY_GREATER_THAN_OR_EQUAL_VALUE", "key": "years", "value": 3}]
			}
		}
	},
	"weekend_sale": {
		"default": false,
		"rules": {
			"saturday and sunday": {
				"when_match": true,
				"conditions": [{"action": "SCHEDULE_BETWEEN_DAYS_OF_WEEK", "key": "CURRENT_DAY_OF_WEEK",
					"value": {"DAYS": ["SATURDAY", "SUNDAY"], "TIMEZONE": "Europe/Rome"}}]
			}
		}
	}
}`

func ExampleFeatureFlags_Evaluate() {
	flags := featureflags.New(memoryStore(schema))
	ctx := context.Background()

	premium, err := flags.Evaluate(ctx, "premium_features", map[string]any{"tier": "premium"}, false)
	if err != nil {
		panic(err)
	}
	standard, err := flags.Evaluate(ctx, "premium_features", map[string]any{"tier": "standard"}, false)
	if err != nil {
		panic(err)
	}
	missing, err := flags.Evaluate(ctx, "no_such_feature", nil, true)
	if err != nil {
		panic(err)
	}
	fmt.Println(premium, standard, missing)
	// Output: true false true
}

func ExampleFeatureFlags_Evaluate_nonBoolean() {
	type discount struct {
		Percent int `json:"percent"`
	}
	flags := featureflags.New(memoryStore(schema))

	got, err := flags.Evaluate(context.Background(), "discount", map[string]any{"years": 5}, discount{})
	if err != nil {
		panic(err)
	}
	fmt.Println(got.Percent)
	// Output: 15
}

func ExampleFeatureFlags_EnabledFeatures() {
	saturday := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	flags := featureflags.New(memoryStore(schema), featureflags.WithClock(func() time.Time { return saturday }))

	enabled, err := flags.EnabledFeatures(context.Background(), map[string]any{"tier": "premium"})
	if err != nil {
		panic(err)
	}
	fmt.Println(enabled)
	// Output: [premium_features ten_percent_off_campaign discount weekend_sale]
}

func ExampleValidate() {
	_, err := featureflags.Validate([]byte(`{"premium_features": {"default": "yes"}}`))
	fmt.Println(err)
	// Output: feature 'default' boolean key must be present, feature=premium_features
}
