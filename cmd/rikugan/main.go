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

	"github.com/Zuful/rikugan/internal/dispatcher"
	"github.com/Zuful/rikugan/internal/provider/argocd"
	"github.com/Zuful/rikugan/internal/provider/github"
	"github.com/Zuful/rikugan/internal/provider/gitlab"
	"github.com/Zuful/rikugan/internal/provider/jaeger"
	"github.com/Zuful/rikugan/internal/provider/kubernetes"
	"github.com/Zuful/rikugan/internal/provider/prometheus"
	"github.com/Zuful/rikugan/internal/provider/runes"
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

	// ── Build clients ─────────────────────────────────────────────
	k8sClient, err := kubernetes.BuildClient("")
	if err != nil {
		return fmt.Errorf("build k8s client: %w", err)
	}

	// ── Providers ──────────────────────────────────────────────────
	k8sProv := kubernetes.NewFromClient(k8sClient, kubernetes.WithLogger(logger))
	if err := d.Register(k8sProv); err != nil {
		return fmt.Errorf("register kubernetes provider: %w", err)
	}

	ghClient, err := github.BuildClient("")
	if err != nil {
		logger.Warn("github provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		ghProv := github.NewFromClient(ghClient, github.WithLogger(logger))
		if err := d.Register(ghProv); err != nil {
			return fmt.Errorf("register github provider: %w", err)
		}
	}

	glClient, err := gitlab.BuildClient("", "")
	if err != nil {
		logger.Warn("gitlab provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		glProv := gitlab.NewFromClient(glClient, gitlab.WithLogger(logger))
		if err := d.Register(glProv); err != nil {
			return fmt.Errorf("register gitlab provider: %w", err)
		}
	}

	argoClient, err := argocd.BuildClient("", "")
	if err != nil {
		logger.Warn("argocd provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		argoProv := argocd.NewFromClient(argoClient, argocd.WithLogger(logger))
		if err := d.Register(argoProv); err != nil {
			return fmt.Errorf("register argocd provider: %w", err)
		}
	}

	jaegerClient, err := jaeger.BuildClient("")
	if err != nil {
		logger.Warn("jaeger provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		jaegerProv := jaeger.NewFromClient(jaegerClient, jaeger.WithLogger(logger))
		if err := d.Register(jaegerProv); err != nil {
			return fmt.Errorf("register jaeger provider: %w", err)
		}
	}

	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		logger.Warn("prometheus provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		promProv := prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger))
		if err := d.Register(promProv); err != nil {
			return fmt.Errorf("register prometheus provider: %w", err)
		}

		// Runes: only when both clients are available
		runesProv := runes.New(k8sClient, promAPI, runes.WithLogger(logger))
		if err := d.Register(runesProv); err != nil {
			return fmt.Errorf("register runes provider: %w", err)
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
