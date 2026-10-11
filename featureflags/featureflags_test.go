package featureflags

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// premiumStore is Python's InMemoryStore of test_comparator_warnings.py.
func premiumStore(action RuleAction, value string) staticStore {
	return staticStore{raw: jsontext.Value(fmt.Sprintf(`{"premium":{"default":false,"rules":{"eligible_customer":{
		"when_match":true,"conditions":[{"key":"customer","action":%q,"value":%s}]}}}}`, action, value))}
}

// recorder collects warnings.
type recorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *recorder) warn(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, message)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...)
}

func TestComparatorFailureWarnsWithoutChangingTheResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action        RuleAction
		context       any
		condition     string
		exceptionType string
		sentinel      error
	}{
		{StartsWith, 123, `"sensitive-condition"`, "AttributeError", ErrOperandNotString},
		{KeyGreaterThanValue, "sensitive-context", `123`, "TypeError", ErrOperandType},
		{AnyInValue, "sensitive-context", `["sensitive-condition"]`, "ValueError", ErrOperandNotList},
	} {
		for _, allFeatures := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/all=%v", tc.action, allFeatures), func(t *testing.T) {
				t.Parallel()
				var warnings recorder
				var failures []*ConditionError
				flags := New(premiumStore(tc.action, tc.condition), WithWarningHandler(warnings.warn))
				evalCtx := map[string]any{"customer": tc.context}
				if allFeatures {
					enabled, err := flags.EnabledFeatures(t.Context(), evalCtx)
					require.NoError(t, err)
					assert.Empty(t, enabled)
				} else {
					got, err := flags.Evaluate(t.Context(), "premium", evalCtx, true)
					require.NoError(t, err)
					assert.False(t, got)
				}
				require.Len(t, warnings.all(), 1)
				message := warnings.all()[0]
				for _, part := range []string{"premium", "eligible_customer", "customer", string(tc.action), tc.exceptionType} {
					assert.Contains(t, message, part)
				}
				assert.NotContains(t, message, "sensitive")

				handled := New(premiumStore(tc.action, tc.condition), WithWarningHandler(func(string) {}),
					WithConditionErrorHandler(func(failure *ConditionError) (bool, error) {
						failures = append(failures, failure)
						return false, nil
					}))
				_, err := handled.Evaluate(t.Context(), "premium", evalCtx, true)
				require.NoError(t, err)
				require.Len(t, failures, 1)
				require.ErrorIs(t, failures[0], tc.sentinel)
				assert.Equal(t, tc.exceptionType, failures[0].ExceptionType)
				assert.NotContains(t, failures[0].Error(), "sensitive-condition")
			})
		}
	}
}

func TestMissingContextAndValidComparisonsDoNotWarn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		context map[string]any
		want    bool
	}{
		{map[string]any{}, false},
		{map[string]any{"customer": "ordinary"}, false},
		{map[string]any{"customer": "premium-customer"}, true},
	} {
		var warnings recorder
		flags := New(premiumStore(StartsWith, `"premium"`), WithWarningHandler(warnings.warn))
		got, err := flags.Evaluate(t.Context(), "premium", tc.context, true)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
		assert.Empty(t, warnings.all())
	}
}

func TestConditionErrorHandler(t *testing.T) {
	t.Parallel()
	var warnings recorder
	handled := 0
	flags := New(premiumStore(StartsWith, `"premium"`), WithWarningHandler(warnings.warn),
		WithConditionErrorHandler(func(failure *ConditionError) (bool, error) {
			handled++
			if !errors.Is(failure, ErrOperandNotString) {
				return false, nil
			}
			return true, nil
		}))
	got, err := flags.Evaluate(t.Context(), "premium", map[string]any{"customer": 123}, false)
	require.NoError(t, err)
	assert.True(t, got, "the handler decides the condition")
	enabled, err := flags.EnabledFeatures(t.Context(), map[string]any{"customer": 123})
	require.NoError(t, err)
	assert.Equal(t, []string{"premium"}, enabled)
	assert.Equal(t, 2, handled)
	assert.Len(t, warnings.all(), 2, "the warning is emitted whatever the handler does")

	errHandler := errors.New("handler failure")
	failing := New(premiumStore(StartsWith, `"premium"`), WithWarningHandler(func(string) {}),
		WithConditionErrorHandler(func(*ConditionError) (bool, error) { return false, errHandler }))
	_, err = failing.Evaluate(t.Context(), "premium", map[string]any{"customer": 123}, false)
	require.ErrorIs(t, err, errHandler)
}

func TestHandlerSeesMissingKeys(t *testing.T) {
	t.Parallel()
	// Python's test_flags_missing_context_key_still_invokes_validation_exception_handler.
	store := staticStore{raw: jsontext.Value(`{"my_feature":{"default":false,"rules":{"tenant_id is in allowed list":{
		"when_match":true,"conditions":[{"action":"ANY_IN_VALUE","key":"tenant_id","value":["Akua","John","Maria","Pat"]}]}}}}`)}
	var warnings recorder
	handled := 0
	flags := New(store, WithWarningHandler(warnings.warn), WithConditionErrorHandler(func(failure *ConditionError) (bool, error) {
		handled++
		return errors.Is(failure, ErrOperandNotList), nil
	}))
	got, err := flags.Evaluate(t.Context(), "my_feature", nil, false)
	require.NoError(t, err)
	assert.True(t, got)
	assert.Equal(t, 1, handled)
	assert.Empty(t, warnings.all(), "a missing key is not worth a warning")

	plain := New(store, WithWarningHandler(func(string) {}))
	got, err = plain.Evaluate(t.Context(), "my_feature", nil, false)
	require.NoError(t, err)
	assert.False(t, got)

	// Python's test_exception_handler: the handler may fail the evaluation.
	errReraised := errors.New("re-raised")
	failing := New(store, WithWarningHandler(func(string) {}), WithConditionErrorHandler(func(*ConditionError) (bool, error) { return false, errReraised }))
	_, err = failing.Evaluate(t.Context(), "my_feature", map[string]any{"tenant_id": "not a list value"}, false)
	require.ErrorIs(t, err, errReraised)
}

func TestEvaluateTypes(t *testing.T) {
	t.Parallel()
	store := staticStore{raw: jsontext.Value(`{
		"tier": {"default": {"group": "read-only", "limit": 10}, "boolean_type": false, "rules": {
			"admins": {"when_match": {"group": "admin", "limit": 1000}, "conditions": [{"action": "EQUALS", "key": "role", "value": "admin"}]}}},
		"limits": {"default": [1, 2], "boolean_type": false},
		"enabled": {"default": true}
	}`)}
	flags := New(store)
	type tier struct {
		Group string `json:"group"`
		Limit int    `json:"limit"`
	}
	got, err := flags.Evaluate(t.Context(), "tier", map[string]any{"role": "admin"}, tier{})
	require.NoError(t, err)
	assert.Equal(t, tier{Group: "admin", Limit: 1000}, got)

	limits, err := flags.Evaluate(t.Context(), "limits", nil, []int{})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, limits, "integers stay integers")

	missing, err := flags.Evaluate(t.Context(), "missing", nil, tier{Group: "fallback"})
	require.NoError(t, err)
	assert.Equal(t, "fallback", missing.Group)

	wrong, err := flags.Evaluate(t.Context(), "enabled", nil, "a string")
	require.ErrorIs(t, err, ErrValueType)
	assert.Equal(t, "a string", wrong, "the default comes back with the error")

	_, err = flags.Evaluate(t.Context(), "enabled", map[string]any{"bad": make(chan int)}, false)
	require.ErrorIs(t, err, ErrInvalidContext)
}

func TestContextIsReadAsJSON(t *testing.T) {
	t.Parallel()
	store := staticStore{raw: jsontext.Value(`{
		"big": {"default": false, "rules": {"r": {"when_match": true, "conditions": [{"action": "EQUALS", "key": "id", "value": 9007199254740993}]}}},
		"tags": {"default": false, "rules": {"r": {"when_match": true, "conditions": [{"action": "ALL_IN_VALUE", "key": "tags", "value": ["a", "b"]}]}}},
		"user": {"default": false, "rules": {"r": {"when_match": true, "conditions": [{"action": "VALUE_IN_KEY", "key": "user", "value": "name"}]}}}
	}`)}
	flags := New(store)
	big, err := flags.Evaluate(t.Context(), "big", map[string]any{"id": int64(9007199254740993)}, false)
	require.NoError(t, err)
	assert.True(t, big, "an int64 beyond 2^53 compares exactly")

	tags, err := flags.Evaluate(t.Context(), "tags", map[string]any{"tags": []string{"a"}}, false)
	require.NoError(t, err)
	assert.True(t, tags, "a Go slice is a JSON list")

	user, err := flags.Evaluate(t.Context(), "user", map[string]any{"user": struct {
		Name string `json:"name"`
	}{"x"}}, false)
	require.NoError(t, err)
	assert.True(t, user, "a struct is a JSON object, its fields its keys")
}

func TestStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	unavailable := New(staticStore{err: &ConfigurationStoreError{Message: "down"}})
	got, err := unavailable.Evaluate(ctx, "x", nil, true)
	require.NoError(t, err)
	assert.True(t, got, "a store failure gives the default")
	enabled, err := unavailable.EnabledFeatures(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, enabled)
	_, err = unavailable.Configuration(ctx)
	require.ErrorIs(t, err, ErrConfigurationStore)

	denied := New(staticStore{err: &StoreClientError{Err: errors.New("AccessDeniedException")}})
	_, err = denied.Evaluate(ctx, "x", nil, true)
	require.ErrorIs(t, err, ErrStoreClient)
	_, err = denied.EnabledFeatures(ctx, nil)
	require.ErrorIs(t, err, ErrStoreClient)

	invalid := New(staticStore{raw: jsontext.Value(`{"x":{"default":"yes"}}`)})
	got, err = invalid.Evaluate(ctx, "x", nil, true)
	require.ErrorIs(t, err, ErrSchemaValidation)
	assert.True(t, got)

	notJSON := New(staticStore{raw: jsontext.Value(`{"x":`)})
	got, err = notJSON.Evaluate(ctx, "x", nil, true)
	require.NoError(t, err, "a document that is not JSON is a store failure")
	assert.True(t, got)
}

// countingStore serves documents and counts the reads.
type countingStore struct {
	mu   sync.Mutex
	docs []string
	read int
}

func (s *countingStore) GetConfiguration(context.Context) (jsontext.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.docs[min(s.read, len(s.docs)-1)]
	s.read++
	return jsontext.Value(doc), nil
}

func TestValidatedOncePerDocument(t *testing.T) {
	t.Parallel()
	store := &countingStore{docs: []string{
		`{"a":{"default":true,"rules":{}}}`,
		`{"a":{"default":true,"rules":{}}}`,
		`{"a":{"default":false,"rules":{}}}`,
	}}
	var warnings recorder
	flags := New(store, WithWarningHandler(warnings.warn))
	for _, want := range []bool{true, true, false, false} {
		got, err := flags.Evaluate(t.Context(), "a", nil, false)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Len(t, warnings.all(), 2, "the empty rules warn once per document")

	doc, err := flags.Configuration(t.Context())
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":{"default":false,"rules":{}}}`, string(doc))
}

func TestRulesInDocumentOrder(t *testing.T) {
	t.Parallel()
	// Both rules match: the first one in the document wins, whatever their
	// names sort to.
	flags := New(staticStore{raw: jsontext.Value(`{"f":{"default":"none","boolean_type":false,"rules":{
		"z rule":{"when_match":"z","conditions":[{"action":"EQUALS","key":"k","value":1}]},
		"a rule":{"when_match":"a","conditions":[{"action":"EQUALS","key":"k","value":1}]}}}}`)})
	got, err := flags.Evaluate(t.Context(), "f", map[string]any{"k": 1}, "")
	require.NoError(t, err)
	assert.Equal(t, "z", got)
}

func TestEnabledFeaturesWithEmptyRulesList(t *testing.T) {
	t.Parallel()
	// Python raises AttributeError here (it calls .items() on the list).
	flags := New(staticStore{raw: jsontext.Value(`{"a":{"default":false,"rules":[]},"b":{"default":true,"rules":null},"c":{"default":true}}`)},
		WithWarningHandler(func(string) {}))
	enabled, err := flags.EnabledFeatures(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c"}, enabled)
}

func TestTimeConditions(t *testing.T) {
	t.Parallel()
	copenhagen, err := time.LoadLocation("Europe/Copenhagen")
	require.NoError(t, err)
	doc := `{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[%s]}}}}`
	for _, tc := range []struct {
		name      string
		condition string
		now       time.Time
		want      bool
	}{
		{"inside a range", `{"action":"SCHEDULE_BETWEEN_TIME_RANGE","key":"CURRENT_TIME","value":{"START":"09:00","END":"17:00"}}`, time.Date(2026, 3, 2, 17, 0, 59, 0, time.UTC), true},
		{"after a range", `{"action":"SCHEDULE_BETWEEN_TIME_RANGE","key":"CURRENT_TIME","value":{"START":"09:00","END":"17:00"}}`, time.Date(2026, 3, 2, 17, 1, 0, 0, time.UTC), false},
		{"across midnight, late", `{"action":"SCHEDULE_BETWEEN_TIME_RANGE","key":"CURRENT_TIME","value":{"START":"23:30","END":"23:00"}}`, time.Date(2026, 3, 2, 23, 45, 0, 0, time.UTC), true},
		{"across midnight, gap", `{"action":"SCHEDULE_BETWEEN_TIME_RANGE","key":"CURRENT_TIME","value":{"START":"23:30","END":"23:00"}}`, time.Date(2026, 3, 2, 23, 15, 0, 0, time.UTC), false},
		{"in a zone", `{"action":"SCHEDULE_BETWEEN_TIME_RANGE","key":"CURRENT_TIME","value":{"START":"09:00","END":"10:00","TIMEZONE":"Europe/Copenhagen"}}`, time.Date(2026, 7, 1, 7, 30, 0, 0, time.UTC), true},
		{"datetime, summer time", `{"action":"SCHEDULE_BETWEEN_DATETIME_RANGE","key":"CURRENT_DATETIME","value":{"START":"2026-07-01T09:00:00","END":"2026-07-01T10:00","TIMEZONE":"Europe/Copenhagen"}}`, time.Date(2026, 7, 1, 9, 30, 0, 0, copenhagen), true},
		{"datetime, end included", `{"action":"SCHEDULE_BETWEEN_DATETIME_RANGE","key":"CURRENT_DATETIME","value":{"START":"2026-07-01","END":"2026-07-01T10:00"}}`, time.Date(2026, 7, 1, 10, 0, 0, 999, time.UTC), true},
		{"datetime, after", `{"action":"SCHEDULE_BETWEEN_DATETIME_RANGE","key":"CURRENT_DATETIME","value":{"START":"2026-07-01","END":"2026-07-01T10:00"}}`, time.Date(2026, 7, 1, 10, 0, 0, 1000, time.UTC), false},
		{"weekday", `{"action":"SCHEDULE_BETWEEN_DAYS_OF_WEEK","key":"CURRENT_DAY_OF_WEEK","value":{"DAYS":["MONDAY","FRIDAY"]}}`, time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), true},
		{"weekday in a zone", `{"action":"SCHEDULE_BETWEEN_DAYS_OF_WEEK","key":"CURRENT_DAY_OF_WEEK","value":{"DAYS":["TUESDAY"],"TIMEZONE":"Pacific/Auckland"}}`, time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flags := New(staticStore{raw: jsontext.Value(fmt.Sprintf(doc, tc.condition))}, WithClock(func() time.Time { return tc.now }))
			got, err := flags.Evaluate(t.Context(), "f", nil, false)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestModuloRange(t *testing.T) {
	t.Parallel()
	flags := New(staticStore{raw: jsontext.Value(`{"f":{"default":false,"rules":{"r":{"when_match":true,
		"conditions":[{"action":"MODULO_RANGE","key":"user","value":{"BASE":100,"START":0,"END":19}}]}}}}`)},
		WithWarningHandler(func(string) {}))
	for _, tc := range []struct {
		user any
		want bool
	}{
		{0, true}, {19, true}, {20, false}, {119, true}, {-81, true}, {-79, false}, {19.5, false}, {true, true}, {"19", false},
	} {
		got, err := flags.Evaluate(t.Context(), "f", map[string]any{"user": tc.user}, false)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%v", tc.user)
	}
}

func TestConcurrentEvaluations(t *testing.T) {
	t.Parallel()
	store := &countingStore{docs: []string{`{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"EQUALS","key":"k","value":1}]}}}}`}}
	flags := New(store)
	var wg sync.WaitGroup
	var matched atomic.Int32
	for i := range 64 {
		wg.Go(func() {
			got, err := flags.Evaluate(t.Context(), "f", map[string]any{"k": i % 2}, false)
			if err != nil {
				t.Error(err)
			}
			if got {
				matched.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(32), matched.Load())
}

func TestValidate(t *testing.T) {
	t.Parallel()
	warnings, err := Validate([]byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"Feature flags schema is empty, no features to validate"}, warnings)

	_, err = Validate([]byte(`not json`))
	require.ErrorIs(t, err, ErrSchemaValidation)

	_, err = Validate([]byte(`{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"EQUALS","key":"k","value":"secret-value"}, {"action":"NOPE","key":"k","value":"secret-value"}]}}}}`))
	require.ErrorIs(t, err, ErrSchemaValidation)
	assert.NotContains(t, err.Error(), "secret-value", "messages never carry condition values")
	assert.True(t, strings.HasPrefix(err.Error(), "'action' value must be either ['EQUALS', "))

	_, err = Validate([]byte(`{"f":{"default":false,"rules":{"r":{"when_match":true,"conditions":[{"action":"SCHEDULE_BETWEEN_TIME_RANGE","key":"CURRENT_TIME","value":{"START":"10:00","END":"11:00","TIMEZONE":5}}]}}}}`))
	require.EqualError(t, err, "'TIMEZONE' must be a string, rule=r", "Python prints <class 'str'> instead of the rule")
}
