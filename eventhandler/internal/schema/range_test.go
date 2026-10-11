package schema

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeRefusesNumbersNoFieldCouldHold(t *testing.T) {
	t.Parallel()
	for _, literal := range []string{"1e999999", "-1e401", "1e-401", "12345e398", strings.Repeat("9", maxNumberLength+1)} {
		_, err := Decode([]byte(`{"n":` + literal + `}`))
		require.ErrorIs(t, err, ErrNumberRange, literal)
	}
	for _, literal := range []string{"1e308", "-2.5e-300", "123456789012345678901234567890", "0.000001", "1E10", "0e700", "-0.0e-999999"} {
		_, err := Decode([]byte(`{"n":` + literal + `}`))
		require.NoError(t, err, literal)
	}
}

func TestHugeExponentsCostNothing(t *testing.T) {
	t.Parallel()
	body := "[" + strings.Repeat("1e999999,", 10000) + "1]"
	start := time.Now()
	_, err := Decode([]byte(body))
	require.ErrorIs(t, err, ErrNumberRange)
	assert.Less(t, time.Since(start), time.Second, "rejected before any exact arithmetic")

	_, ok := rat(Number("1e999999"))
	assert.False(t, ok)

	start = time.Now()
	r, ok := rat(Number("-0.0e-999999"))
	require.True(t, ok)
	assert.Equal(t, 0, r.Sign())
	assert.Less(t, time.Since(start), 10*time.Millisecond, "a zero needs no exponent")
}
