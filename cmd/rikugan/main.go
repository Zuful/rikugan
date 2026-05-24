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
