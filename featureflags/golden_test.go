package featureflags

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The golden cases are every evaluation and validation the upstream test
// suite (tests/functional/feature_flags of Powertools Python v3.35.0)
// performs, recorded while it ran: the schema, the context, the clock and
// what Python returned or raised. The Go engine must give the same answer
// for each of them.
const goldenFile = "testdata/python_cases.jsonl"

// staticStore serves a fixed document, or fails.
type staticStore struct {
	raw jsontext.Value
	err error
}

func (s staticStore) GetConfiguration(context.Context) (jsontext.Value, error) {
	return s.raw, s.err
}

type goldenCase struct {
	line int
	obj  *object
}

func (c goldenCase) str(name string) string {
	return asString(c.obj.getOr(name, ""))
}

// asString returns v when it is a string, "" otherwise.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// as returns v as a T, failing the test when it is not one.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	typed, ok := v.(T)
	require.True(t, ok, "%v is a %T", v, v)
	return typed
}

func (c goldenCase) json(t *testing.T, name string) jsontext.Value {
	t.Helper()
	raw, err := encodeJSON(c.obj.getOr(name, nil))
	require.NoError(t, err)
	return raw
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	f, err := os.Open(goldenFile)
	require.NoError(t, err)
	defer func() { _ = f.Close() }() //nolint:errcheck // a read-only file has nothing to report on close
	var cases []goldenCase
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		value, err := decodeJSON(scanner.Bytes())
		require.NoError(t, err, "line %d", line)
		obj, ok := value.(*object)
		require.True(t, ok)
		cases = append(cases, goldenCase{line: line, obj: obj})
	}
	require.NoError(t, scanner.Err())
	require.Greater(t, len(cases), 250)
	return cases
}

func TestGoldenPythonCases(t *testing.T) {
	t.Parallel()
	for _, c := range loadGolden(t) {
		name := c.str("kind") + "/" + c.str("test")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			switch c.str("kind") {
			case "evaluate", "enabled":
				checkEvaluation(t, c)
			case "validate":
				checkValidation(t, c)
			case "static":
				checkStatic(t, c)
			case "envelope":
				checkEnvelope(t, c)
			default:
				t.Fatalf("line %d: unknown kind", c.line)
			}
		})
	}
}

func checkEvaluation(t *testing.T, c goldenCase) {
	t.Helper()
	if c.str("test") == "test_schema_invalid_document_is_never_cached_as_validated" {
		t.Skip("the StopIteration of an exhausted mock, not a behavior")
	}
	store := staticStore{}
	switch c.str("store_error") {
	case "":
		store.raw = c.json(t, "config")
	case "ConfigurationStoreError":
		store.err = &ConfigurationStoreError{Message: "Unable to get AWS AppConfig configuration file"}
	case "StoreClientError":
		store.err = &StoreClientError{Err: errors.New("AccessDeniedException")}
	default:
		t.Fatalf("line %d: unknown store error %q", c.line, c.str("store_error"))
	}
	opts := []Option{WithWarningHandler(func(string) {})}
	if now, ok := c.obj.getOr("now", nil).(*object); ok {
		at, err := time.Parse(time.RFC3339Nano, asString(now.getOr("iso", "")))
		require.NoError(t, err)
		opts = append(opts, WithClock(func() time.Time { return at }))
	}
	flags := New(store, opts...)
	var evalCtx map[string]any
	require.NoError(t, json.Unmarshal(c.json(t, "context"), &evalCtx))

	var got any
	var err error
	if c.str("kind") == "evaluate" {
		var def any
		require.NoError(t, json.Unmarshal(c.json(t, "default"), &def))
		got, err = flags.Evaluate(t.Context(), c.str("name"), evalCtx, def)
	} else {
		got, err = flags.EnabledFeatures(t.Context(), evalCtx)
	}
	if pyErr, ok := c.obj.getOr("error", nil).(*object); ok {
		require.Error(t, err, "line %d", c.line)
		if pyErr.getOr("type", "") == "StoreClientError" {
			require.ErrorIs(t, err, ErrStoreClient)
		}
		return
	}
	require.NoError(t, err, "line %d", c.line)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(c.json(t, "result")), string(gotJSON), "line %d", c.line)
}

func checkValidation(t *testing.T, c goldenCase) {
	t.Helper()
	warnings, err := Validate(c.json(t, "schema"))
	if want := c.str("error"); want != "" {
		require.Error(t, err, "line %d", c.line)
		require.ErrorIs(t, err, ErrSchemaValidation)
		assert.Equal(t, pythonFixed(want), err.Error(), "line %d", c.line)
		return
	}
	require.NoError(t, err, "line %d", c.line)
	var want []string
	for _, w := range as[[]any](t, c.obj.getOr("warnings", []any{})) {
		want = append(want, asString(w))
	}
	assert.Equal(t, want, warnings, "line %d", c.line)
}

// pythonFixed returns a Python message as this package writes it, where it
// fixes a bug of the message itself.
func pythonFixed(message string) string {
	// Python formats the builtin str instead of the rule name.
	return strings.ReplaceAll(message, "'TIMEZONE' must be a string, rule=<class 'str'>", "'TIMEZONE' must be a string, rule=dummy")
}

func checkStatic(t *testing.T, c goldenCase) {
	t.Helper()
	args := as[*object](t, c.obj.getOr("args", nil))
	ruleName := asString(args.getOr("rule_name", ""))
	featureName := asString(args.getOr("feature_name", ""))
	condition, isObject := args.getOr("condition", nil).(*object)
	if !isObject {
		condition = newObject()
	}
	var err error
	switch c.str("fn") {
	case "ConditionsValidator.validate_condition":
		err = validateCondition(ruleName, args.getOr("condition", nil))
	case "ConditionsValidator.validate_condition_action":
		err = validateConditionAction(condition, ruleName)
	case "ConditionsValidator.validate_condition_key":
		err = validateConditionKey(condition, ruleName)
	case "ConditionsValidator.validate_condition_value":
		err = validateConditionValue(condition, ruleName)
	case "RulesValidator.validate_rule":
		_, err = validateRule(args.getOr("rule", nil), ruleName, featureName, truthy(args.getOr("boolean_feature", true)))
	case "RulesValidator.validate_rule_name":
		err = validateRuleName(ruleName, featureName)
	default:
		t.Fatalf("line %d: unknown validator %q", c.line, c.str("fn"))
	}
	if want := c.str("error"); want != "" {
		require.Error(t, err, "line %d", c.line)
		assert.Equal(t, pythonFixed(want), err.Error(), "line %d", c.line)
		return
	}
	require.NoError(t, err, "line %d", c.line)
}

func checkEnvelope(t *testing.T, c goldenCase) {
	t.Helper()
	got, err := extract(c.json(t, "raw"), c.str("envelope"), nil)
	require.NoError(t, err)
	assert.Equal(t, string(c.json(t, "result")), string(got), "line %d: same members in the same order", c.line)
}
