// Package diag configures Sutracoder's structured diagnostics log — a
// separate slog stream written to a file (never the TUI), capturing loop
// decisions and provider metadata for post-hoc debugging. Keys are never
// logged.
package diag

import (
	"log/slog"
	"os"
	"path/filepath"
)

// NewFileLogger opens (creating parent dirs) a JSON slog logger writing to
// path at debug level. On failure it returns a logger writing to stderr so
// diagnostics are never silently dropped.
func NewFileLogger(path string) *slog.Logger {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
