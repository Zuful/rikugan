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
	"github.com/Zuful/rikugan/internal/provider"
	"github.com/Zuful/rikugan/internal/provider/argocd"
	"github.com/Zuful/rikugan/internal/provider/falco"
	"github.com/Zuful/rikugan/internal/provider/github"
	"github.com/Zuful/rikugan/internal/provider/gitlab"
	"github.com/Zuful/rikugan/internal/provider/jaeger"
	"github.com/Zuful/rikugan/internal/provider/kubernetes"
	"github.com/Zuful/rikugan/internal/provider/loki"
	"github.com/Zuful/rikugan/internal/provider/prometheus"
	"github.com/Zuful/rikugan/internal/provider/runes"
	"github.com/Zuful/rikugan/internal/provider/sentry"
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

	// ── Kubernetes is required: failure here aborts startup. ────────
	k8sClient, err := kubernetes.BuildClient("")
	if err != nil {
		return fmt.Errorf("build k8s client: %w", err)
	}
	if err := d.Register(kubernetes.NewFromClient(k8sClient, kubernetes.WithLogger(logger))); err != nil {
		return fmt.Errorf("register kubernetes provider: %w", err)
	}

	// ── Optional providers: missing config logs a warning, not fatal. ───
	type optionalProvider struct {
		name  string
		setup func() (provider.Provider, error)
	}

	optionals := []optionalProvider{
		{"github", func() (provider.Provider, error) {
			c, err := github.BuildClient("")
			if err != nil {
				return nil, err
			}
			return github.NewFromClient(c, github.WithLogger(logger)), nil
		}},
		{"gitlab", func() (provider.Provider, error) {
			c, err := gitlab.BuildClient("", "")
			if err != nil {
				return nil, err
			}
			return gitlab.NewFromClient(c, gitlab.WithLogger(logger)), nil
		}},
		{"argocd", func() (provider.Provider, error) {
			c, err := argocd.BuildClient("", "")
			if err != nil {
				return nil, err
			}
			return argocd.NewFromClient(c, argocd.WithLogger(logger)), nil
		}},
		{"jaeger", func() (provider.Provider, error) {
			c, err := jaeger.BuildClient("")
			if err != nil {
				return nil, err
			}
			return jaeger.NewFromClient(c, jaeger.WithLogger(logger)), nil
		}},
		{"falco", func() (provider.Provider, error) {
			c, err := falco.BuildClient("")
			if err != nil {
				return nil, err
			}
			return falco.NewFromClient(c, falco.WithLogger(logger)), nil
		}},
		{"loki", func() (provider.Provider, error) {
			c, err := loki.BuildClient("")
			if err != nil {
				return nil, err
			}
			return loki.NewFromClient(c, loki.WithLogger(logger)), nil
		}},
		{"sentry", func() (provider.Provider, error) {
			c, err := sentry.BuildClient("", "", "")
			if err != nil {
				return nil, err
			}
			return sentry.NewFromClient(c, sentry.WithLogger(logger)), nil
		}},
	}

	for _, opt := range optionals {
		if err := registerOptional(d, logger, opt.name, opt.setup); err != nil {
			return err
		}
	}

	// ── Prometheus + Runes: runes is only valid when both k8s and prom
	// are present, so they share a setup block. ───────────────────
	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		logger.Warn("prometheus provider not available, skipping",
			slog.String("reason", err.Error()),
		)
	} else {
		if err := d.Register(prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger))); err != nil {
			return fmt.Errorf("register prometheus provider: %w", err)
		}
		if err := d.Register(runes.New(k8sClient, promAPI, runes.WithLogger(logger))); err != nil {
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

// registerOptional runs setup() and registers the resulting provider. If
// setup fails (typically because the required env var is unset), it logs a
// warning and returns nil — startup proceeds without that provider. A
// dispatcher-side registration error is fatal and returned wrapped.
func registerOptional(
	d *dispatcher.Dispatcher,
	logger *slog.Logger,
	name string,
	setup func() (provider.Provider, error),
) error {
	p, err := setup()
	if err != nil {
		logger.Warn(name+" provider not available, skipping",
			slog.String("reason", err.Error()),
		)
		return nil
	}
	if err := d.Register(p); err != nil {
		return fmt.Errorf("register %s provider: %w", name, err)
	}
	return nil
}
