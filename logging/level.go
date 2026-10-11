package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// The levels slog lacks and Lambda knows. LevelCritical is Powertools
// Python's name for LevelFatal.
const (
	LevelTrace    slog.Level = slog.LevelDebug - 4
	LevelFatal    slog.Level = slog.LevelError + 4
	LevelCritical            = LevelFatal
)

// The level names Lambda reads.
const (
	nameTrace = "TRACE"
	nameDebug = "DEBUG"
	nameInfo  = "INFO"
	nameWarn  = "WARN"
	nameError = "ERROR"
	nameFatal = "FATAL"
)

// lambdaLevels maps Lambda's level names to levels.
var lambdaLevels = map[string]slog.Level{
	nameTrace: LevelTrace,
	nameDebug: slog.LevelDebug,
	nameInfo:  slog.LevelInfo,
	nameWarn:  slog.LevelWarn,
	nameError: slog.LevelError,
	nameFatal: LevelFatal,
}

// ErrInvalidLevel is returned by ParseLevel for a name it does not know.
var ErrInvalidLevel = errors.New("invalid log level")

// ParseLevel reads a level name, in any case: the Lambda names (TRACE,
// DEBUG, INFO, WARN, ERROR, FATAL), the Python ones (WARNING, CRITICAL,
// NOTSET, which logs everything), and slog's offsets such as "INFO+2".
func ParseLevel(name string) (slog.Level, error) {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if level, ok := lambdaLevels[upper]; ok {
		return level, nil
	}
	switch upper {
	case "NOTSET":
		return LevelTrace, nil
	case "WARNING":
		return slog.LevelWarn, nil
	case "CRITICAL":
		return LevelFatal, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidLevel, name)
	}
	return level, nil
}

// LambdaLevelName names a level the way Lambda's log-level filtering reads
// it: TRACE, DEBUG, INFO, WARN, ERROR or FATAL. A level between two names
// takes the lower one (INFO+2 is INFO), since Lambda files an unknown name
// under INFO, which would let an ERROR+2 record through a WARN filter as
// INFO and drop it.
func LambdaLevelName(level slog.Level) string {
	switch {
	case level < slog.LevelDebug:
		return nameTrace
	case level < slog.LevelInfo:
		return nameDebug
	case level < slog.LevelWarn:
		return nameInfo
	case level < slog.LevelError:
		return nameWarn
	case level < LevelFatal:
		return nameError
	default:
		return nameFatal
	}
}

// PythonLevelName names a level the way Powertools Python does: DEBUG,
// INFO, WARNING, ERROR or CRITICAL, and TRACE below DEBUG. Pass it to
// WithLevelNames when Go and Python functions share log queries on level
// names. Lambda's log-level filtering does not know WARNING and CRITICAL.
func PythonLevelName(level slog.Level) string {
	switch name := LambdaLevelName(level); name {
	case nameWarn:
		return "WARNING"
	case nameFatal:
		return "CRITICAL"
	default:
		return name
	}
}

// alcLevel reads the level Lambda's Advanced Logging Controls enforce, set
// in AWS_LAMBDA_LOG_LEVEL, with the names Lambda uses. An unknown value is
// ignored, as Powertools Python ignores it.
func alcLevel() (slog.Level, bool) {
	level, ok := lambdaLevels[strings.ToUpper(env.Get(env.LambdaLogLevel))]
	return level, ok
}

// configuredLevel is the level the function asks for: explicit, else
// POWERTOOLS_LOG_LEVEL, else the legacy LOG_LEVEL. A variable whose value is
// not a level is skipped, with a warning.
func configuredLevel(explicit *slog.Level) (level slog.Level, ok bool, warnings []string) {
	if explicit != nil {
		return *explicit, true, nil
	}
	for _, name := range []string{env.LogLevel, env.LegacyLogLevel} {
		raw := env.Get(name)
		if raw == "" {
			continue
		}
		level, err := ParseLevel(raw)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Ignoring %s=%q: it is not a log level.", name, raw))
			continue
		}
		return level, true, warnings
	}
	return 0, false, warnings
}

// resolveLevel decides the logger's level as Powertools Python does: Lambda's
// Advanced Logging Controls first, since Lambda drops whatever is below them
// anyway; then the configured level; then INFO. When the configured level is
// more verbose than what Lambda keeps, it warns that records will be lost.
func resolveLevel(explicit *slog.Level) (level slog.Level, warnings []string) {
	alc, hasALC := alcLevel()
	configured, hasConfigured, warnings := configuredLevel(explicit)
	if hasALC && hasConfigured && alc > configured {
		warnings = append(warnings, fmt.Sprintf("Current log level (%s) does not match AWS Lambda Advanced Logging Controls "+
			"minimum log level (%s). This can lead to data loss, consider adjusting them.",
			LambdaLevelName(configured), LambdaLevelName(alc)))
	}
	switch {
	case hasALC:
		return alc, warnings
	case hasConfigured:
		return configured, warnings
	default:
		return slog.LevelInfo, warnings
	}
}
