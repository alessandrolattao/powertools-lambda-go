package warn

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnce(t *testing.T) { //nolint:paralleltest // warnings are captured process-wide
	var out bytes.Buffer
	restore := Capture(&out)
	defer restore()

	Once(t.Context(), UserWarning, "first")
	Once(t.Context(), UserWarning, "first")
	Once(t.Context(), PowertoolsUserWarning, "first")
	Once(t.Context(), UserWarning, "second", "key", "a")
	Once(t.Context(), UserWarning, "second", "key", "b")
	Once(t.Context(), UserWarning, "second", "key", "a")

	var records []map[string]any
	for line := range strings.Lines(out.String()) {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}
	require.Len(t, records, 4)
	assert.Equal(t, "WARN", records[0]["level"])
	assert.Equal(t, "first", records[0]["msg"])
	assert.Equal(t, "UserWarning", records[0]["category"])
	assert.Equal(t, "PowertoolsUserWarning", records[1]["category"])
	assert.Equal(t, "b", records[3]["key"])
}
