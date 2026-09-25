// Command metrics-mcp starts the Rikugan metrics MCP server over
// STDIO transport — the concept-only binary introduced in étape 4 of
// the refactor.
//
// Tools exposed: get_metric, compare_to_baseline, list_available_metrics
// (canonical concept tools). The runes correlation tool (six_eyes)
// is intentionally excluded — it depends on both kubernetes and
// prometheus and lives only in the unified binary.
//
// Requires the configured metrics backend env var (today: PROMETHEUS_URL).
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

	mcpSrv, err := app.SetupMetrics(logger, version)
	if err != nil {
		return err
	}

	stdio := server.NewStdioServer(mcpSrv)
	logger.Info("rikugan-metrics starting",
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
