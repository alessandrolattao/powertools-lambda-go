package disabled

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIs(t *testing.T) { //nolint:paralleltest // it sets environment variables
	tests := []struct {
		disabled, dev string
		want          bool
	}{
		{"", "", false},
		{"true", "", true},
		{"TRUE", "false", true},
		{"false", "", false},
		{"", "true", true},
		{"", "1", true},
		{"false", "true", false},
		{"0", "on", false},
		{"no", "no", false},
		{"bogus", "", false},
	}
	for _, tt := range tests {
		t.Setenv("POWERTOOLS_METRICS_DISABLED", tt.disabled)
		t.Setenv("POWERTOOLS_DEV", tt.dev)
		assert.Equal(t, tt.want, Is(), "disabled=%q dev=%q", tt.disabled, tt.dev)
	}
}
