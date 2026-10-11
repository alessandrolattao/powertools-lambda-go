package featureflags

import (
	"bufio"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The comparator table is what Python's RULE_ACTION_MAPPING answers for
// every pair of a set of JSON values (numbers, booleans, null, strings,
// lists, objects), generated with Powertools Python v3.35.0. Each answer is
// one letter: T and F for a result, t, a and v for the TypeError,
// AttributeError and ValueError the comparator raises.
const comparatorsFile = "testdata/python_comparators.jsonl"

func outcome(matched bool, err error) string {
	if err != nil {
		var pyErr *pyError
		if !errors.As(err, &pyErr) {
			return "?"
		}
		return map[string]string{"TypeError": "t", "AttributeError": "a", "ValueError": "v"}[pyErr.class]
	}
	if matched {
		return "T"
	}
	return "F"
}

func TestComparatorsAgreeWithPython(t *testing.T) {
	t.Parallel()
	f, err := os.Open(comparatorsFile)
	require.NoError(t, err)
	defer func() { _ = f.Close() }() //nolint:errcheck // a read-only file has nothing to report on close
	scanner := bufio.NewScanner(f)
	require.True(t, scanner.Scan())
	header, err := decodeJSON(scanner.Bytes())
	require.NoError(t, err)
	names := as[[]any](t, as[*object](t, header).getOr("actions", nil))
	actions := make([]RuleAction, 0, len(names))
	for _, a := range names {
		actions = append(actions, RuleAction(asString(a)))
	}
	require.Len(t, actions, 17)

	rows := 0
	for scanner.Scan() {
		rows++
		value, err := decodeJSON(scanner.Bytes())
		require.NoError(t, err)
		row := as[*object](t, value)
		a, b := row.getOr("a", nil), row.getOr("b", nil)
		if want, ok := row.get("modulo"); ok {
			assert.Equal(t, want, outcome(compare(ModuloRange, a, b, time.Time{})), "%s %% %s", pyRepr(a), pyRepr(b))
			continue
		}
		want := asString(row.getOr("r", ""))
		for i, action := range actions {
			assert.Equal(t, want[i:i+1], outcome(compare(action, a, b, time.Time{})), "%s %s %s", pyRepr(a), action, pyRepr(b))
		}
	}
	require.NoError(t, scanner.Err())
	assert.Greater(t, rows, 600)
}

func TestPythonRepr(t *testing.T) {
	t.Parallel()
	value, err := decodeJSON([]byte(`{"s":"it's","q":"say \"hi\"","n":[1,2.5,1e20,1e-5,true,null],"u":"é\u0001"}`))
	require.NoError(t, err)
	assert.Equal(t, `{'s': "it's", 'q': 'say "hi"', 'n': [1, 2.5, 1e+20, 1e-05, True, None], 'u': 'é\x01'}`, pyRepr(value))
	assert.Equal(t, "plain", pyStr("plain"))
}
