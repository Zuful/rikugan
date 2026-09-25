// Command alerts-mcp starts the Rikugan alerts MCP server over STDIO
// transport — the concept-only binary introduced in étape 4 of the
// refactor.
//
// Tools exposed: list_active_alerts, get_alert_details (canonical
// concept tools routed through the ALERTS_PROVIDER-selected backend)
// plus each backend's residual non-canonical tools:
// list_issues_for_release (sentry) when SENTRY_AUTH_TOKEN is set,
// get_current_oncalls (pagerduty) when PAGERDUTY_API_TOKEN is set.
//
// Configure at least one backend via its env vars (ALERTMANAGER_URL,
// SENTRY_URL+SENTRY_AUTH_TOKEN+SENTRY_ORG, PAGERDUTY_API_TOKEN). Set
// ALERTS_PROVIDER to pick which backend serves the concept tools
// (default: priority order alertmanager > sentry > pagerduty).
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

	mcpSrv, err := app.SetupAlerts(logger, version)
	if err != nil {
		return err
	}

	stdio := server.NewStdioServer(mcpSrv)
	logger.Info("rikugan-alerts starting",
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
