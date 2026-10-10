// Package debug is where Powertools writes down what it does internally.
//
// It is silent unless POWERTOOLS_DEBUG is on, as Powertools Python's own
// package logger is.
package debug

import (
	"log/slog"
	"os"
	"sync"

	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
)

// Logger returns the internal logger: JSON records on standard error at Debug
// when POWERTOOLS_DEBUG is on, nothing otherwise.
var Logger = sync.OnceValue(func() *slog.Logger {
	if !env.IsDebug() {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).
		With("logger", "powertools")
})
