package warn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOnceRemembersTheCause(t *testing.T) {
	t.Parallel()
	Once("warn-test-cause", "first")
	_, loaded := seen.Load("warn-test-cause")
	assert.True(t, loaded)
	Once("warn-test-cause", "second") // silent: already reported
}
