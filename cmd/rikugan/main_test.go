package main

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want slog.Level
	}{
		{"empty defaults to info", "", slog.LevelInfo},
		{"info lower", "info", slog.LevelInfo},
		{"INFO upper", "INFO", slog.LevelInfo},
		{"  info  with whitespace", "  info  ", slog.LevelInfo},
		{"debug", "debug", slog.LevelDebug},
		{"Debug mixed case", "Debug", slog.LevelDebug},
		{"warn", "warn", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"unknown falls back to info", "verbose", slog.LevelInfo},
		{"numeric falls back to info", "42", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseLogLevel(tt.in); got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
