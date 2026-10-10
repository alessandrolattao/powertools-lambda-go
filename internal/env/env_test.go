package env

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseBool(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"1", "y", "YES", "t", "True", "on", " true "} {
		value, ok := ParseBool(raw)
		assert.True(t, ok, raw)
		assert.True(t, value, raw)
	}
	for _, raw := range []string{"0", "n", "No", "f", "FALSE", "off"} {
		value, ok := ParseBool(raw)
		assert.True(t, ok, raw)
		assert.False(t, value, raw)
	}
	for _, raw := range []string{"", "maybe", "2", "enabled"} {
		_, ok := ParseBool(raw)
		assert.False(t, ok, raw)
	}
}

func TestBool(t *testing.T) {
	t.Setenv("PT_TEST_BOOL", "")
	assert.True(t, Bool("PT_TEST_BOOL", true), "unset gives the fallback")

	t.Setenv("PT_TEST_BOOL", "off")
	assert.False(t, Bool("PT_TEST_BOOL", true))

	t.Setenv("PT_TEST_BOOL", "nonsense")
	assert.True(t, Bool("PT_TEST_BOOL", true), "an invalid value gives the fallback")
}

func TestFloat(t *testing.T) {
	t.Setenv("PT_TEST_FLOAT", "0.25")
	assert.InDelta(t, 0.25, Float("PT_TEST_FLOAT", 1), 1e-9)

	t.Setenv("PT_TEST_FLOAT", "a quarter")
	assert.InDelta(t, 1.0, Float("PT_TEST_FLOAT", 1), 1e-9)
}

func TestSeconds(t *testing.T) {
	t.Setenv("PT_TEST_SECONDS", "300")
	assert.Equal(t, 5*time.Minute, Seconds("PT_TEST_SECONDS", time.Second))

	t.Setenv("PT_TEST_SECONDS", "-1")
	assert.Equal(t, time.Second, Seconds("PT_TEST_SECONDS", time.Second))

	t.Setenv("PT_TEST_SECONDS", "5m")
	assert.Equal(t, time.Second, Seconds("PT_TEST_SECONDS", time.Second))
}

func TestServiceNameOr(t *testing.T) {
	t.Setenv(ServiceName, "")
	assert.Equal(t, DefaultServiceName, ServiceNameOr(DefaultServiceName))

	t.Setenv(ServiceName, " payment ")
	assert.Equal(t, "payment", ServiceNameOr(DefaultServiceName))
}

func TestIsProvisioned(t *testing.T) {
	t.Setenv(LambdaInitializationType, "on-demand")
	assert.False(t, IsProvisioned())

	t.Setenv(LambdaInitializationType, "provisioned-concurrency")
	assert.True(t, IsProvisioned())
}
