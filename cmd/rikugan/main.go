// Command rikugan starts the Rikugan MCP server over STDIO transport.
//
// Logs are written as structured JSON to stderr (the STDIO MCP transport
// uses stdin/stdout for the JSON-RPC protocol).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/app"
)

// version is set at build-time via -ldflags.
var version = "0.1.0"

func main() {
	// Structured JSON logger → stderr so it never collides with the
	// STDIO transport on stdout.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(os.Getenv("RIKUGAN_LOG_LEVEL")),
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// parseLogLevel maps a human-readable level string to slog.Level.
// Accepted values (case-insensitive): debug, info, warn, error.
// Unrecognised or empty values default to INFO.
func parseLogLevel(s string) slog.Level {
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

// run wires the cancellable context, asks the app package to build the
// MCP server with every available provider, and serves it over STDIO
// until the listener exits or a signal fires.
func run(logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	mcpSrv, err := app.Setup(logger, version)
	if err != nil {
		return err
	}

	stdio := server.NewStdioServer(mcpSrv)
	logger.Info("rikugan starting",
		slog.String("transport", "stdio"),
		slog.String("version", version),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- stdio.Listen(ctx, os.Stdin, os.Stdout)
	}()

	select {
	case <-ctx.Done():
		logger.Info("received shutdown signal, exiting")
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("stdio listener: %w", err)
		}
		return nil
	}
}
