// Package log provides a thin wrapper over log/slog with level parsing, so the
// rest of the app depends on one logging surface rather than slog directly.
package log

import (
	"log/slog"
	"os"
	"strings"
)

// New builds a JSON structured logger at the given level ("debug", "info",
// "warn", "error", or "silent").
func New(level string) *slog.Logger {
	if strings.EqualFold(level, "silent") {
		return slog.New(slog.NewJSONHandler(discard{}, nil))
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)})
	return slog.New(h)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "trace", "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
