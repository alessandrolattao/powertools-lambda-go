package featureflags

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// Store provides the feature flags document: Python's StoreProvider. A
// custom store (S3, a database, a file) implements it.
//
// GetConfiguration returns the schema as raw JSON, after any envelope.
// Features and rules are evaluated in the order the document lists them. A
// store reports a document it cannot provide with a *ConfigurationStoreError
// (Evaluate then returns the caller's default) and a refusal the caller must
// hear about with a *StoreClientError; other errors are returned as they
// are. FeatureFlags validates a document once and reuses it while the store
// returns the same bytes.
type Store interface {
	GetConfiguration(ctx context.Context) (jsontext.Value, error)
}

// FeatureFlags evaluates feature flags against a context: Python's
// FeatureFlags. It is safe for concurrent use: create it once and share it
// across invocations.
type FeatureFlags struct {
	store   Store
	now     func() time.Time
	handler func(*ConditionError) (bool, error)
	warn    func(string)

	mu      sync.Mutex
	lastRaw []byte
	last    *object
}

// Option configures a FeatureFlags.
type Option func(*FeatureFlags)

// WithClock replaces the clock of the time conditions, for tests.
func WithClock(now func() time.Time) Option {
	return func(f *FeatureFlags) { f.now = now }
}

// WithConditionErrorHandler decides what a condition whose operands cannot
// be compared evaluates to: Python's validation_exception_handler. The
// handler receives the failure (errors.Is tells ErrOperandType,
// ErrOperandNotString and ErrOperandNotList apart, as Python's handlers are
// registered per exception class) and returns whether the condition
// matches; an error it returns fails the evaluation. Without a handler such
// a condition does not match.
//
// As in Python, the handler also sees the failures of conditions whose key
// is missing from the context, and may make them match.
func WithConditionErrorHandler(handler func(failure *ConditionError) (matched bool, err error)) Option {
	return func(f *FeatureFlags) { f.handler = handler }
}

// WithWarningHandler receives the warnings Python emits as
// PowertoolsUserWarning: an empty schema, a feature with empty rules, a
// condition that could not be evaluated. By default each distinct warning
// is written once, as a JSON record on standard error where Lambda collects
// it, the way Python shows a warning once per place.
func WithWarningHandler(handler func(message string)) Option {
	return func(f *FeatureFlags) { f.warn = handler }
}

// New returns a FeatureFlags that reads its schema from store.
func New(store Store, opts ...Option) *FeatureFlags {
	f := &FeatureFlags{store: store, now: time.Now}
	for _, opt := range opts {
		opt(f)
	}
	if f.warn == nil {
		f.warn = onceToStderr()
	}
	return f
}

// onceToStderr writes each distinct warning once, on standard error.
func onceToStderr() func(string) {
	var seen sync.Map
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	return func(message string) {
		if _, loaded := seen.LoadOrStore(message, struct{}{}); !loaded {
			logger.Warn("powertools: feature flags", "warning", message)
		}
	}
}

// configuration returns the validated document, fetching it from the store
// and validating it when the store returns new bytes.
func (f *FeatureFlags) configuration(ctx context.Context) (*object, error) {
	raw, err := f.store.GetConfiguration(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.last != nil && bytes.Equal(raw, f.lastRaw) {
		doc := f.last
		f.mu.Unlock()
		debug.Logger().Debug("feature flags schema already validated, skipping validation")
		return doc, nil
	}
	f.mu.Unlock()

	doc, err := decodeJSON(raw)
	if err != nil {
		return nil, &ConfigurationStoreError{Message: "Unable to parse the feature flags configuration", Err: err}
	}
	features, warnings, err := validateSchema(doc)
	for _, w := range warnings {
		f.warn(w)
	}
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.lastRaw, f.last = bytes.Clone(raw), features
	f.mu.Unlock()
	return features, nil
}

// Configuration returns the validated schema, as JSON: Python's
// get_configuration. A failure of the store is returned as it is, here.
func (f *FeatureFlags) Configuration(ctx context.Context) (jsontext.Value, error) {
	doc, err := f.configuration(ctx)
	if err != nil {
		return nil, err
	}
	return encodeJSON(doc)
}

// Evaluate returns the value of the feature name for the context evalCtx:
// Python's evaluate.
//
//   - The feature exists and a rule matches: the rule's when_match.
//   - The feature exists, it has no rules or none matches: its default.
//   - The feature does not exist, or the store could not provide the
//     schema (ErrConfigurationStore): def, with a nil error.
//
// A boolean feature (the default kind) gives a bool; one with "boolean_type":
// false gives any JSON value, which is decoded into T. A value that does not
// fit T returns def and a *ValueTypeError. A schema that does not validate
// and a store that refuses the request return def and the error. evalCtx
// may hold any value that marshals to JSON; nil is an empty context.
func (f *FeatureFlags) Evaluate[T any](ctx context.Context, name string, evalCtx map[string]any, def T) (T, error) {
	values, err := normalizeContext(evalCtx)
	if err != nil {
		return def, err
	}
	features, err := f.configuration(ctx)
	if err != nil {
		if errors.Is(err, ErrConfigurationStore) {
			debug.Logger().Debug("failed to fetch feature flags from store, returning default provided", "err", err)
			return def, nil
		}
		return def, err
	}
	value, found, err := f.evaluate(features, name, values)
	if err != nil || !found {
		return def, err
	}
	out, err := decodeAs[T](value)
	if err != nil {
		return def, &ValueTypeError{Feature: name, Err: err}
	}
	return out, nil
}

// EnabledFeatures returns the names of the features that are enabled for
// evalCtx, in document order: Python's get_enabled_features. A feature is
// enabled when its default is truthy and it has no rules, or when its rules
// give a truthy value; so a non-boolean feature whose value is a non-empty
// list or object counts, as in Python. When the store cannot provide the
// schema the list is empty.
func (f *FeatureFlags) EnabledFeatures(ctx context.Context, evalCtx map[string]any) ([]string, error) {
	values, err := normalizeContext(evalCtx)
	if err != nil {
		return nil, err
	}
	features, err := f.configuration(ctx)
	if err != nil {
		if errors.Is(err, ErrConfigurationStore) {
			return []string{}, nil
		}
		return nil, err
	}
	return f.enabledFeatures(features, values)
}

// evaluate is Python's evaluate once the schema is in hand.
func (f *FeatureFlags) evaluate(features *object, name string, values *object) (value any, found bool, err error) {
	raw, ok := features.get(name)
	if !ok || raw == nil {
		return nil, false, nil
	}
	feature, ok := raw.(*object)
	if !ok {
		return nil, false, nil
	}
	rules := feature.getOr(keyRules, nil)
	def := feature.getOr(keyDefault, nil)
	booleanFeature := truthy(feature.getOr(keyBooleanType, true))
	if !truthy(rules) {
		if booleanFeature {
			return truthy(def), true, nil
		}
		return def, true, nil
	}
	value, err = f.evaluateRules(name, values, def, rules, booleanFeature)
	return value, err == nil, err
}

// enabledFeatures is Python's get_enabled_features once the schema is in
// hand.
func (f *FeatureFlags) enabledFeatures(features, values *object) ([]string, error) {
	enabled := []string{}
	for _, name := range features.keys {
		feature, ok := features.values[name].(*object)
		if !ok {
			continue
		}
		rules := feature.getOr(keyRules, newObject())
		def := feature.getOr(keyDefault, nil)
		booleanFeature := truthy(feature.getOr(keyBooleanType, true))
		if truthy(def) && !truthy(rules) {
			enabled = append(enabled, name)
			continue
		}
		value, err := f.evaluateRules(name, values, def, rules, booleanFeature)
		if err != nil {
			return nil, err
		}
		if truthy(value) {
			enabled = append(enabled, name)
		}
	}
	return enabled, nil
}

// evaluateRules returns the when_match of the first rule whose conditions
// all match, as a bool for a boolean feature, or the feature's default.
// Python fails here when rules is an empty list or null and the default is
// falsy (it calls .items() on them); both are read as no rules.
func (f *FeatureFlags) evaluateRules(featureName string, values *object, def, rules any, booleanFeature bool) (any, error) {
	obj, ok := rules.(*object)
	if !ok {
		return def, nil
	}
	for _, ruleName := range obj.keys {
		rule, ok := obj.values[ruleName].(*object)
		if !ok {
			continue
		}
		matched, err := f.matchConditions(featureName, ruleName, rule, values)
		if err != nil {
			return nil, err
		}
		if matched {
			whenMatch := rule.getOr(keyWhenMatch, nil)
			if booleanFeature {
				return truthy(whenMatch), nil
			}
			return whenMatch, nil
		}
	}
	return def, nil
}

// matchConditions reports whether every condition of the rule matches.
func (f *FeatureFlags) matchConditions(featureName, ruleName string, rule, values *object) (bool, error) {
	conditions, _ := rule.getOr(keyConditions, nil).([]any) //nolint:errcheck // a rule without a list of conditions never matches
	if len(conditions) == 0 {
		return false, nil
	}
	for _, raw := range conditions {
		condition, ok := raw.(*object)
		if !ok {
			return false, nil
		}
		matched, err := f.matchCondition(featureName, ruleName, condition, values)
		if err != nil || !matched {
			return false, err
		}
	}
	return true, nil
}

// matchCondition is Python's _match_by_action for one condition.
func (f *FeatureFlags) matchCondition(featureName, ruleName string, condition, values *object) (bool, error) {
	key, _ := condition.getOr(keyKey, "").(string)       //nolint:errcheck // validation checked the type
	action, _ := condition.getOr(keyAction, "").(string) //nolint:errcheck // validation checked the type
	value := condition.getOr(keyValue, nil)

	var contextValue any = key
	present := true
	switch RuleAction(action) {
	case ScheduleBetweenTimeRange, ScheduleBetweenDatetimeRange, ScheduleBetweenDaysOfWeek:
		// Time conditions have no context: the key names the clock.
	default:
		contextValue, present = values.get(key)
	}

	matched, err := compare(RuleAction(action), contextValue, value, f.now())
	if err == nil {
		// A key missing from the context never matches, whatever the
		// action: NOT_EQUALS would otherwise match a missing key.
		return matched && present, nil
	}
	condErr := &ConditionError{Feature: featureName, Rule: ruleName, Key: key, Action: action, ExceptionType: exceptionType(err), Err: err}
	debug.Logger().Debug("caught exception while matching action", "action", action, "err", err)
	if present {
		// The warning names the condition, never the operands nor the
		// message, which may hold customer data.
		f.warn(fmt.Sprintf("Failed to evaluate feature flag condition: feature=%s, rule=%s, key=%s, action=%s, exception_type=%s",
			featureName, ruleName, key, action, condErr.ExceptionType))
	}
	if f.handler == nil {
		return false, nil
	}
	return f.handler(condErr)
}

func exceptionType(err error) string {
	if e, ok := errors.AsType[*pyError](err); ok {
		return e.class
	}
	return "Exception"
}

// normalizeContext turns the caller's context into the value model, through
// JSON, so that every Go type compares as the JSON value it stands for and
// integers stay exact.
func normalizeContext(evalCtx map[string]any) (*object, error) {
	if len(evalCtx) == 0 {
		return newObject(), nil
	}
	raw, err := json.Marshal(evalCtx, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidContext, err)
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidContext, err)
	}
	obj, ok := value.(*object)
	if !ok {
		return nil, fmt.Errorf("%w: not an object", ErrInvalidContext)
	}
	return obj, nil
}

// decodeAs turns a model value into a T, through JSON.
func decodeAs[T any](value any) (T, error) {
	var out T
	if b, ok := value.(bool); ok {
		if typed, ok := any(b).(T); ok {
			return typed, nil
		}
	}
	raw, err := encodeJSON(value)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode %s into %T: %w", raw, out, err)
	}
	return out, nil
}
