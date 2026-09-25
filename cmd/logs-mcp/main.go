// Command logs-mcp starts the Rikugan logs MCP server over STDIO
// transport — the concept-only binary introduced in étape 4 of the
// refactor.
//
// Tools exposed: get_logs, tail_logs, search_logs (canonical concept
// tools) plus detect_log_anomalies (loki-specific analytics tool).
// Requires the configured logs backend env var (today: LOKI_URL).
//
// Logs are written as structured JSON to stderr; stdin/stdout carry
// the JSON-RPC protocol.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/app"
	"github.com/Zuful/rikugan/pkg/logging"
)

// version is set at build-time via -ldflags.
var version = "0.1.0"

func main() {
	logger := logging.New("RIKUGAN_LOG_LEVEL")
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	mcpSrv, err := app.SetupLogs(logger, version)
	if err != nil {
		return err
	}

	stdio := server.NewStdioServer(mcpSrv)
	logger.Info("rikugan-logs starting",
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
