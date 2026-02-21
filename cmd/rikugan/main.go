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

	"github.com/Zuful/rikugan/internal/dispatcher"
	"github.com/Zuful/rikugan/internal/provider/kubernetes"
	"github.com/Zuful/rikugan/internal/provider/prometheus"
)

// version is set at build-time via -ldflags.
var version = "0.1.0"

func main() {
	// Structured JSON logger → stderr so it never collides with the
	// STDIO transport on stdout.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("server exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run encapsulates all server logic so it stays testable and never calls
// os.Exit directly.
func run(logger *slog.Logger) error {
	// ── Context with signal-based cancellation ──────────────────────
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	// ── MCP server ──────────────────────────────────────────────────
	mcpSrv := server.NewMCPServer(
		"rikugan",
		version,
		server.WithToolCapabilities(false), // no dynamic tool list changes yet
		server.WithRecovery(),              // recover panics inside handlers
	)

	// ── Dispatcher ──────────────────────────────────────────────────
	d := dispatcher.New(mcpSrv, logger)

	// ── Providers ──────────────────────────────────────────────────
	k8s, err := kubernetes.New(
		kubernetes.WithLogger(logger),
	)
	if err != nil {
		return fmt.Errorf("init kubernetes provider: %w", err)
	}
	if err := d.Register(k8s); err != nil {
		return fmt.Errorf("register kubernetes provider: %w", err)
	}

	prom, err := prometheus.New(prometheus.WithLogger(logger))
	if err != nil {
		logger.Warn("prometheus provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		if err := d.Register(prom); err != nil {
			return fmt.Errorf("register prometheus provider: %w", err)
		}
	}

	// ── STDIO transport ─────────────────────────────────────────────
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
