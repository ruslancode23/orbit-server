// Package logger builds the process-wide structured logger.
package logger

import (
	"log/slog"
	"os"
)

// New returns a human-readable logger in development and JSON in production.
func New(development bool) *slog.Logger {
	if development {
		return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
