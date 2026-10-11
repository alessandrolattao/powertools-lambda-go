package logging

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want slog.Level
	}{
		{"TRACE", LevelTrace},
		{"notset", LevelTrace},
		{"debug", slog.LevelDebug},
		{" Info ", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"fatal", LevelFatal},
		{"CRITICAL", LevelCritical},
		{"INFO+2", slog.LevelInfo + 2},
		{"debug-4", slog.LevelDebug - 4},
	}
	for _, tc := range cases {
		got, err := ParseLevel(tc.name)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
	for _, name := range []string{"", "verbose", "INFO+x"} {
		_, err := ParseLevel(name)
		require.ErrorIs(t, err, ErrInvalidLevel, name)
	}
}

func TestLevelNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level          slog.Level
		lambda, python string
	}{
		{LevelTrace - 1, "TRACE", "TRACE"},
		{LevelTrace, "TRACE", "TRACE"},
		{slog.LevelDebug, "DEBUG", "DEBUG"},
		{slog.LevelInfo, "INFO", "INFO"},
		{slog.LevelInfo + 2, "INFO", "INFO"},
		{slog.LevelWarn, "WARN", "WARNING"},
		{slog.LevelError, "ERROR", "ERROR"},
		{slog.LevelError + 2, "ERROR", "ERROR"},
		{LevelFatal, "FATAL", "CRITICAL"},
		{LevelFatal + 8, "FATAL", "CRITICAL"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.lambda, LambdaLevelName(tc.level), tc.level)
		assert.Equal(t, tc.python, PythonLevelName(tc.level), tc.level)
	}
}

// TestLevelPrecedence follows Powertools Python: AWS_LAMBDA_LOG_LEVEL, then
// the explicit level, then POWERTOOLS_LOG_LEVEL, then LOG_LEVEL, then INFO.
func TestLevelPrecedence(t *testing.T) {
	explicit := func(level slog.Level) *slog.Level { return &level }
	cases := []struct {
		name                    string
		alc, powertools, legacy string
		explicit                *slog.Level
		want                    slog.Level
		warning                 string
	}{
		{name: "default", want: slog.LevelInfo},
		{name: "legacy variable", legacy: "DEBUG", want: slog.LevelDebug},
		{name: "Powertools variable over legacy", powertools: "WARN", legacy: "DEBUG", want: slog.LevelWarn},
		{name: "Python names", powertools: "critical", want: LevelFatal},
		{name: "explicit over variables", powertools: "DEBUG", explicit: explicit(slog.LevelError), want: slog.LevelError},
		{
			name: "ALC over explicit, with a warning", alc: "WARN", explicit: explicit(slog.LevelDebug), want: slog.LevelWarn,
			warning: "Current log level (DEBUG) does not match AWS Lambda Advanced Logging Controls minimum log level (WARN)",
		},
		{
			name: "ALC over variable, with a warning", alc: "ERROR", powertools: "INFO", want: slog.LevelError,
			warning: "Current log level (INFO) does not match",
		},
		{name: "ALC less strict, no warning", alc: "DEBUG", explicit: explicit(slog.LevelError), want: slog.LevelDebug},
		{name: "ALC alone", alc: "TRACE", want: LevelTrace},
		{name: "ALC fatal", alc: "FATAL", want: LevelFatal},
		{name: "unknown ALC is ignored", alc: "LOUD", powertools: "ERROR", want: slog.LevelError},
		{
			name: "invalid variable warns and falls through", powertools: "LOUD", legacy: "WARN", want: slog.LevelWarn,
			warning: `Ignoring POWERTOOLS_LOG_LEVEL=\"LOUD\": it is not a log level.`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_LAMBDA_LOG_LEVEL", tc.alc)
			t.Setenv("POWERTOOLS_LOG_LEVEL", tc.powertools)
			t.Setenv("LOG_LEVEL", tc.legacy)
			var opts []Option
			if tc.explicit != nil {
				opts = append(opts, WithLevel(*tc.explicit))
			}
			f := newFixture(t, opts...)
			assert.Equal(t, tc.want, f.logger.Level())
			if tc.warning == "" {
				assert.Empty(t, f.warnings.String())
				return
			}
			assert.Contains(t, f.warnings.String(), tc.warning)
			assert.Contains(t, f.warnings.String(), `"level":"WARN"`)
		})
	}
}

func TestSetLevel(t *testing.T) {
	t.Setenv("AWS_LAMBDA_LOG_LEVEL", "")
	t.Setenv("POWERTOOLS_LOG_LEVEL", "")
	t.Setenv("LOG_LEVEL", "")
	f := newFixture(t)
	assert.Equal(t, slog.LevelInfo, f.logger.Level())
	f.logger.SetLevel(slog.LevelDebug)
	assert.Equal(t, slog.LevelDebug, f.logger.Level())
	assert.True(t, f.logger.Enabled(t.Context(), slog.LevelDebug))

	t.Setenv("AWS_LAMBDA_LOG_LEVEL", "WARN")
	f.logger.SetLevel(slog.LevelDebug)
	assert.Equal(t, slog.LevelWarn, f.logger.Level(), "Lambda's level wins over SetLevel")
	assert.Contains(t, f.warnings.String(), "This can lead to data loss")
}

func TestWarningsOncePerCause(t *testing.T) {
	t.Setenv("AWS_LAMBDA_LOG_LEVEL", "ERROR")
	f := newFixture(t, WithLevel(slog.LevelDebug))
	f.logger.SetLevel(slog.LevelDebug)
	f.logger.SetLevel(slog.LevelDebug)
	assert.Len(t, f.warnings.lines(), 1)
}
