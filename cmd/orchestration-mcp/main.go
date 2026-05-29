// Command orchestration-mcp starts the Rikugan orchestration MCP
// server over STDIO transport — the concept-only binary introduced in
// étape 4 of the refactor.
//
// Tools exposed: list_services, get_service_state, describe_service,
// get_recent_events, restart_service, get_service_history (canonical
// concept tools) plus find_pods_by_service_name (kubernetes-specific
// discovery helper). The kubernetes client is required; configure it
// via KUBECONFIG (in-cluster auth detected automatically when running
// inside a pod).
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

	mcpSrv, err := app.SetupOrchestration(logger, version)
	if err != nil {
		return err
	}

	stdio := server.NewStdioServer(mcpSrv)
	logger.Info("rikugan-orchestration starting",
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
