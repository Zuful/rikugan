// Package app wires the rikugan MCP server end-to-end: it constructs
// the dispatcher, builds every provider, and returns a server ready to
// be served over any transport. It exists so cmd/rikugan/main.go can
// stay minimal — log init, signal context, app.Setup, transport — and
// so a future second binary under cmd/ could share the same wiring.
package app

import (
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/dispatcher"
	"github.com/Zuful/rikugan/internal/provider/kubernetes"
	"github.com/Zuful/rikugan/internal/provider/prometheus"
	"github.com/Zuful/rikugan/internal/provider/runes"
)

// Setup constructs an MCP server, registers every available provider
// via a dispatcher, and returns the server ready to be wrapped in any
// transport (today only STDIO, tomorrow potentially HTTP or others).
//
// Provider availability is environment-driven: kubernetes is required
// and its absence is fatal; every other provider degrades to warn-and-
// skip if its env config is missing. Prometheus and runes are coupled —
// runes needs both kubernetes and prometheus to be present, so it
// registers only when prometheus is.
func Setup(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := server.NewMCPServer(
		"rikugan",
		version,
		server.WithToolCapabilities(false), // no dynamic tool list changes yet
		server.WithRecovery(),              // recover panics inside handlers
	)

	d := dispatcher.New(mcpSrv, logger)

	// ── Kubernetes is required: failure here aborts startup. ────────
	k8sClient, err := kubernetes.BuildClient("")
	if err != nil {
		return nil, fmt.Errorf("build k8s client: %w", err)
	}
	if err := d.Register(kubernetes.NewFromClient(k8sClient, kubernetes.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register kubernetes provider: %w", err)
	}

	// ── Optional providers: missing config = warn-and-skip. ────────
	for _, opt := range optionalProviders(logger) {
		if err := registerOptional(d, logger, opt.name, opt.setup); err != nil {
			return nil, err
		}
	}

	// ── Prometheus + Runes: coupled — runes needs both k8s and prom. ───
	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		logger.Warn("prometheus provider not available, skipping",
			slog.String("reason", err.Error()),
		)
		return mcpSrv, nil
	}
	if err := d.Register(prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register prometheus provider: %w", err)
	}
	if err := d.Register(runes.New(k8sClient, promAPI, runes.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register runes provider: %w", err)
	}

	return mcpSrv, nil
}
