// Package featureflags evaluates feature flags: a small rule engine that
// decides, from a JSON schema and the context of a request, whether a
// feature is on, or which value it takes. It is the Go side of Powertools'
// Feature Flags utility, and reads the same schema.
//
// # Getting started
//
// A FeatureFlags reads its schema from a Store, by default an AWS AppConfig
// freeform configuration profile. Create both once and share them:
//
//	var flags = featureflags.New(
//		featureflags.AppConfigStoreFromConfig(cfg, "product-catalogue", "dev", "features"),
//	)
//
//	func handle(ctx context.Context, event Event) (Response, error) {
//		premium, err := flags.Evaluate(ctx, "premium_features",
//			map[string]any{"tier": event.Tier}, false)
//		...
//	}
//
// Evaluate returns, for the feature name and the context:
//
//   - the when_match of the first rule whose conditions all match;
//   - the feature's default when it has no rules or none matches;
//   - the caller's default when the feature does not exist, or when the store
//     could not provide the schema (an unreachable AppConfig).
//
// The type is inferred from the default: a bool for a boolean feature, any
// Go type the JSON value of a non-boolean feature ("boolean_type": false)
// decodes into. EnabledFeatures returns the names of the features enabled for
// a context.
//
// # Schema
//
//	{
//	  "premium_features": {
//	    "default": false,
//	    "rules": {
//	      "customer tier equals premium": {
//	        "when_match": true,
//	        "conditions": [{"action": "EQUALS", "key": "tier", "value": "premium"}]
//	      }
//	    }
//	  }
//	}
//
// Rules are tried in the order the document lists them; the conditions of a
// rule must all match. A condition whose key is missing from the context
// never matches, whatever its action. The actions are the RuleAction
// constants, with Python's semantics on JSON values: numbers compare by value
// whatever their type (true equals 1), strings by code point, lists member
// by member; KEY_IN_VALUE and its relatives test membership in a list, a
// substring of a string, or a key of an object. The time actions read the
// clock in the condition's TIMEZONE (an IANA name, UTC by default) from an
// embedded zone database, so they work on any Lambda runtime; WithClock
// replaces the clock in tests. MODULO_RANGE uses Python's floored modulo.
//
// Validate checks a schema the way Python's SchemaValidator does, with its
// messages, which never include condition values; use it in a test or a CI
// step. FeatureFlags validates each new document once.
//
// # Stores
//
// Store is one method, GetConfiguration, which returns the schema as raw JSON.
// AppConfigStore reads it through package parameters/appconfig, caching it
// for five seconds (WithMaxAge) and never polling sooner than AppConfig
// allows. WithEnvelope selects the schema inside a larger document with a
// JMESPath expression (package jmespath, with the Powertools functions);
// GetRawConfiguration returns the whole document. An AppConfig access denied
// is a *StoreClientError, returned by Evaluate; any other failure to read the
// document is a *ConfigurationStoreError, which Evaluate turns into the
// caller's default.
//
// # Errors and warnings
//
// A condition whose operands cannot be compared (a number for STARTSWITH, a
// string for ANY_IN_VALUE) does not match. Python emits a
// PowertoolsUserWarning for it: here the warning goes to the handler of
// WithWarningHandler, by default once per distinct message as a JSON record
// on standard error. WithConditionErrorHandler, Python's
// validation_exception_handler, may decide the condition instead, or fail the
// evaluation; it receives a *ConditionError that matches ErrOperandType,
// ErrOperandNotString or ErrOperandNotList (Python's TypeError,
// AttributeError and ValueError).
//
// # Differences from Python
//
//   - Evaluate is generic and returns an error: a value that does not fit
//     the type asked for is a *ValueTypeError, the default coming back with
//     it.
//   - A document is validated again only when its bytes change; Python
//     compares object identity.
//   - The envelope keeps the order of the document's objects, as Python's
//     dicts do. Objects the expression builds itself (powertools_json of a
//     string, a multiselect) come out with their keys sorted.
//   - Context values are read through JSON: a Go struct is an object, a slice
//     a list. Integers are exact up to int64; Python's are unbounded.
//   - Fixed: EnabledFeatures treats "rules": [] or null as no rules; Python
//     raises AttributeError when the default is false.
//   - Fixed: the "'TIMEZONE' must be a string" message names the rule;
//     Python prints the str builtin.
//   - TIMEZONE must be an IANA zone name; Python's dateutil also accepts
//     POSIX TZ strings ("EST5EDT"). ISO datetimes follow Python 3.10's
//     fromisoformat, the oldest Python Powertools supports.
//   - Validate returns the warnings instead of emitting them; an action that
//     is not a string fails validation instead of raising TypeError.
//   - The AppConfig "Feature Flags" profile type is not supported, as in
//     Python; the botocore user agent registration is not ported.
package featureflags
