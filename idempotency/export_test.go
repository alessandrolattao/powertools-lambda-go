package idempotency

import (
	"log/slog"
	"time"
)

// WithClock makes the idempotent function read the time from now, so that
// tests can move it.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// WithWarnings sends the warnings to logger instead of standard error.
func WithWarnings(logger *slog.Logger) Option {
	return func(c *config) { c.warnings = logger }
}

// CanonicalJSON exposes the canonical encoder to the fuzz test.
func CanonicalJSON(raw []byte) ([]byte, error) {
	tree, err := decodeTree(raw)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(tree)
}
