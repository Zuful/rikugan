// Package logging hosts the slog initialiser and log-level parser
// that every rikugan binary uses. Centralising them here means the
// unified rikugan binary and the per-concept binaries
// (rikugan-orchestration / rikugan-logs / rikugan-metrics /
// rikugan-alerts) don't each re-implement the same JSON-on-stderr
// boilerplate.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns the canonical rikugan logger: structured JSON written to
// stderr (stdout is reserved for the STDIO MCP transport's JSON-RPC).
// The level is taken from the RIKUGAN_LOG_LEVEL env var if envVar is
// empty.
func New(envVar string) *slog.Logger {
	if envVar == "" {
		envVar = "RIKUGAN_LOG_LEVEL"
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: ParseLevel(os.Getenv(envVar)),
	}))
}

// ParseLevel maps a human-readable level string to slog.Level.
// Accepted values (case-insensitive, whitespace-tolerant): debug, info,
// warn, error. Unrecognised or empty values default to INFO.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
