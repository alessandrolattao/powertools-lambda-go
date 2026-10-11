# Feature flags

The feature flags utility is a simple rule engine that decides when one or
many features are enabled for a request, from a JSON schema and the request's
context. It reads the schema Powertools Python reads, and gives the same
answers for it.

Package: `featureflags`. The default store is an AWS AppConfig freeform
configuration profile.

## Key features

* Define simple feature flags to dynamically decide when to enable a feature
* Fetch one or all feature flags enabled for a given application context
* Support for static feature flags to simply turn a feature on or off
* Support for time based feature flags, in any time zone
* Non-boolean feature flags: any JSON value, decoded into a Go type
* Bring your own feature flags store

## Terminology

**Static flags** are simply on or off: `TRACER_ENABLED=true`.

**Dynamic flags** have varying states: enable premium features for customer
X, not Y.

You can use the [parameters](parameters.md) utility for static flags, while
this utility does both static and dynamic ones. Feature flags add complexity
over time: use them sparingly.

AWS AppConfig requires two API calls to fetch a configuration the first time.
Consolidate your feature flags in a single configuration profile.

## Getting started

### IAM permissions

With `AppConfigStore`, the function's role needs
`appconfig:StartConfigurationSession` and `appconfig:GetLatestConfiguration`.

### Required resources

An AppConfig application, an environment, a freeform configuration profile
holding the schema, and a deployment of it:

```json
{
  "premium_features": {
    "default": false,
    "rules": {
      "customer tier equals premium": {
        "when_match": true,
        "conditions": [{"action": "EQUALS", "key": "tier", "value": "premium"}]
      }
    }
  },
  "ten_percent_off_campaign": {
    "default": false
  }
}
```

### Evaluating a single feature flag

Create the store and the flags once, outside the handler, and evaluate in it.
The type of the result is the type of the default.

```go
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/alessandrolattao/powertools-lambda-go/featureflags"
)

type order struct {
	Tier string `json:"tier"`
}

var flags *featureflags.FeatureFlags

func handler(ctx context.Context, event order) (string, error) {
	premium, err := flags.Evaluate(ctx, "premium_features", map[string]any{"tier": event.Tier}, false)
	if err != nil {
		return "", err
	}
	if premium {
		return "premium features enabled", nil
	}
	return "standard", nil
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	store := featureflags.AppConfigStoreFromConfig(cfg, "product-catalogue", "dev", "features")
	flags = featureflags.New(store)
	lambda.Start(handler)
}
```

`Evaluate` returns the `when_match` of the first rule whose conditions all
match, else the feature's `default`. When the feature does not exist, or the
store cannot provide the schema (AppConfig unreachable), it returns your
default with no error, as Python does. An access denied, or a schema that does
not validate, returns your default and the error.

#### Static flags

A feature without rules is static: its `default` is the answer.

```go
discount, err := flags.Evaluate(ctx, "ten_percent_off_campaign", nil, false)
```

### Getting all enabled features

```go
enabled, err := flags.EnabledFeatures(ctx, map[string]any{"tier": "premium"})
// ["premium_features", "ten_percent_off_campaign"]
```

A feature is enabled when its default is true and it has no rules, or when its
rules give a true value. A non-boolean feature counts when its value is
"truthy" in Python's sense: a non-empty list, object or string, a non-zero
number.

### Time based feature flags

`SCHEDULE_BETWEEN_TIME_RANGE`, `SCHEDULE_BETWEEN_DATETIME_RANGE` and
`SCHEDULE_BETWEEN_DAYS_OF_WEEK` read the clock instead of the context, in the
condition's `TIMEZONE` (an IANA zone name, UTC by default). The zone database
is embedded, so they work on every Lambda runtime.

```json
{
  "weekend_premium_discount": {
    "default": false,
    "rules": {
      "customer tier equals premium and its time for a discount": {
        "when_match": true,
        "conditions": [
          {"action": "EQUALS", "key": "tier", "value": "premium"},
          {"action": "SCHEDULE_BETWEEN_DAYS_OF_WEEK", "key": "CURRENT_DAY_OF_WEEK",
           "value": {"DAYS": ["SATURDAY", "SUNDAY"], "TIMEZONE": "America/New_York"}}
        ]
      }
    }
  }
}
```

A time range whose `END` comes before its `START` crosses midnight
(`"START": "22:00", "END": "06:00"`). Datetime ranges are ISO 8601 without an
offset; the zone is `TIMEZONE`, so that daylight saving time is accounted for.

### Modulo range segmented experimentation

`MODULO_RANGE` enables a feature for a stable share of users:
`START <= key % BASE <= END`.

```json
{
  "new_checkout": {
    "default": false,
    "rules": {
      "ten percent of users": {
        "when_match": true,
        "conditions": [{"action": "MODULO_RANGE", "key": "user_id", "value": {"BASE": 100, "START": 0, "END": 9}}]
      }
    }
  }
}
```

### Beyond boolean feature flags

With `"boolean_type": false`, `default` and `when_match` can be any JSON
value, which `Evaluate` decodes into the type of your default.

```json
{
  "limits": {
    "default": {"requests_per_minute": 10},
    "boolean_type": false,
    "rules": {
      "enterprise customers": {
        "when_match": {"requests_per_minute": 1000},
        "conditions": [{"action": "KEY_IN_VALUE", "key": "tier", "value": ["enterprise", "partner"]}]
      }
    }
  }
}
```

```go
type limits struct {
	RequestsPerMinute int `json:"requests_per_minute"`
}
got, err := flags.Evaluate(ctx, "limits", map[string]any{"tier": "partner"}, limits{RequestsPerMinute: 1})
```

A value that does not fit the type returns your default and a
`*featureflags.ValueTypeError`.

## Advanced

### Adjusting in-memory cache

`AppConfigStore` keeps the configuration for five seconds before asking
AppConfig again; it never polls sooner than the interval AppConfig asks for.

```go
store := featureflags.AppConfigStoreFromConfig(cfg, "product-catalogue", "dev", "features",
	featureflags.WithMaxAge(5*time.Minute))
```

### Getting fetched configuration

When the profile holds application configuration next to the feature flags,
`GetRawConfiguration` returns the whole document, without another call.

```go
store := featureflags.AppConfigStoreFromConfig(cfg, "product-catalogue", "dev", "configuration",
	featureflags.WithEnvelope("features"))
raw, err := store.GetRawConfiguration(ctx)
```

### Schema

#### Features

A feature has a `default`, a boolean unless `"boolean_type": false`, and
optional `rules`.

#### Rules

Each rule has a name (its key), a `when_match` value (a boolean for boolean
features) and a list of `conditions`. Rules are tried in the order the
document lists them; the first that matches wins.

#### Conditions

A condition has an `action`, a `key` (looked up in the context) and a
`value`. All the conditions of a rule must match. A condition whose key is
missing from the context never matches, whatever its action: a `NOT_EQUALS`
on `tier` does not match a request without a `tier`.

| Action | Matches when, with `a` the context value and `b` the condition value |
| --- | --- |
| `EQUALS` | `a == b` |
| `NOT_EQUALS` | `a != b` |
| `KEY_GREATER_THAN_VALUE` | `a > b` |
| `KEY_GREATER_THAN_OR_EQUAL_VALUE` | `a >= b` |
| `KEY_LESS_THAN_VALUE` | `a < b` |
| `KEY_LESS_THAN_OR_EQUAL_VALUE` | `a <= b` |
| `STARTSWITH` | the string `a` starts with `b` |
| `ENDSWITH` | the string `a` ends with `b` |
| `KEY_IN_VALUE` (`IN`) | `a` is in `b` |
| `KEY_NOT_IN_VALUE` (`NOT_IN`) | `a` is not in `b` |
| `ANY_IN_VALUE` | any member of the list `a` is in `b` |
| `ALL_IN_VALUE` | every member of the list `a` is in `b` |
| `NONE_IN_VALUE` | no member of the list `a` is in `b` |
| `VALUE_IN_KEY` | `b` is in `a` |
| `VALUE_NOT_IN_KEY` | `b` is not in `a` |
| `SCHEDULE_BETWEEN_TIME_RANGE` | `START <= time of day <= END` (key `CURRENT_TIME`) |
| `SCHEDULE_BETWEEN_DATETIME_RANGE` | `START <= now <= END` (key `CURRENT_DATETIME`) |
| `SCHEDULE_BETWEEN_DAYS_OF_WEEK` | today is one of `DAYS` (key `CURRENT_DAY_OF_WEEK`) |
| `MODULO_RANGE` | `START <= a % BASE <= END` |

Comparisons follow Python on JSON values: numbers compare by value whatever
their type (`true` equals `1`), strings by code point, lists member by member;
"in" means membership in a list, a substring of a string, or a key of an
object. The context is read through JSON, so a Go struct is an object and a
slice a list.

A comparison that cannot be made (a number for `STARTSWITH`, a string for
`ANY_IN_VALUE`) does not match, and emits a warning naming the feature, the
rule, the key, the action and the kind of failure, never the values. By
default each distinct warning is written once to standard error;
`WithWarningHandler` receives them instead. `WithConditionErrorHandler`
decides what such a condition evaluates to, or fails the evaluation:

```go
flags := featureflags.New(store, featureflags.WithConditionErrorHandler(
	func(failure *featureflags.ConditionError) (bool, error) {
		if errors.Is(failure, featureflags.ErrOperandNotList) {
			return false, fmt.Errorf("feature %s: %w", failure.Feature, failure)
		}
		return false, nil
	}))
```

#### Validating a schema

`Validate` checks a schema as Python's `SchemaValidator` does, with its
messages; run it in a test or in CI before deploying a configuration.

```go
warnings, err := featureflags.Validate(document)
```

### Envelope

When the feature flags are part of a larger configuration, `WithEnvelope`
selects them with a JMESPath expression; the Powertools functions
(`powertools_json`, `powertools_base64`, `powertools_base64_gzip`) are
available, and `WithJMESPathFunctions` adds your own.

```json
{
  "logging": {"level": "INFO"},
  "features": {
    "premium_features": {"default": false}
  }
}
```

```go
store := featureflags.AppConfigStoreFromConfig(cfg, "product-catalogue", "dev", "configuration",
	featureflags.WithEnvelope("features"))
```

### Built-in store provider

#### AppConfig

`NewAppConfigStore` takes an AppConfigData client (`appconfig.Client`, which
`*appconfigdata.Client` satisfies); `AppConfigStoreFromConfig` builds one from
an `aws.Config`, for another region or role. It reads through
`parameters/appconfig`, which keeps the configuration session of the profile.
Only freeform profiles are supported, as in Python.

### Create your own store provider

A store is one method that returns the schema as JSON: read it from S3, a
database, or, as here, a file shipped with the function.

```go
package main

import (
	"context"
	"encoding/json/jsontext"
	"os"

	"github.com/alessandrolattao/powertools-lambda-go/featureflags"
)

// fileStore reads the schema from a file.
type fileStore struct {
	path string
}

func (s fileStore) GetConfiguration(context.Context) (jsontext.Value, error) {
	body, err := os.ReadFile(s.path)
	if err != nil {
		// Evaluate turns this error into the caller's default.
		return nil, &featureflags.ConfigurationStoreError{Message: "unable to read the feature flags", Err: err}
	}
	return body, nil
}

func main() {
	_ = featureflags.New(fileStore{path: "features.json"})
}
```

A `*featureflags.ConfigurationStoreError` makes `Evaluate` return the caller's
default; a `*featureflags.StoreClientError` (an access denied) and any other
error reach the caller.

`FeatureFlags` validates a document once, and again only when the store
returns different bytes; caching is up to the store.

## Testing your code

Evaluate your rules without AppConfig: give `FeatureFlags` a store that
returns the schema, and a clock for the time conditions.

```go
package main

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/featureflags"
)

type fixedStore string

func (s fixedStore) GetConfiguration(context.Context) (jsontext.Value, error) {
	return jsontext.Value(s), nil
}

func main() {
	schema := fixedStore(`{"premium_features": {"default": false, "rules": {"premium": {
		"when_match": true, "conditions": [{"action": "EQUALS", "key": "tier", "value": "premium"}]}}}}`)
	monday := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	flags := featureflags.New(schema, featureflags.WithClock(func() time.Time { return monday }))

	premium, err := flags.Evaluate(context.Background(), "premium_features", map[string]any{"tier": "premium"}, false)
	fmt.Println(premium, err) // true <nil>
}
```

## Feature flags vs parameters vs environment variables

| Method | When to use | Requires a deployment on changes | Services |
| --- | --- | --- | --- |
| Environment variables | Simple configuration that rarely changes | Yes | Lambda |
| [Parameters](parameters.md) | Secrets, or parameters in different formats | No | Parameter Store, DynamoDB, Secrets Manager, AppConfig |
| Feature flags | A rule engine deciding when features are enabled for the input | No | AppConfig |

## Differences from Python

* `Evaluate` is generic: the default sets the type, and a value that does not
  fit it is a `*ValueTypeError`. Errors are returned, never raised.
* `validation_exception_handler` is `WithConditionErrorHandler`; the
  `PowertoolsUserWarning`s go to `WithWarningHandler`.
* A schema is validated again when its bytes change, not its identity.
* Objects that the envelope expression builds itself (through
  `powertools_json`, or a multiselect) come out with their keys sorted; the
  document's own objects keep their order.
* `TIMEZONE` takes IANA zone names, not POSIX TZ strings.
* Fixed: `EnabledFeatures` treats `"rules": []` and `"rules": null` as no
  rules, where Python raises.
