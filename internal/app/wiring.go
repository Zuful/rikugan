// Package app wires the rikugan MCP server end-to-end: it constructs
// the dispatcher, builds every provider, and returns a server ready to
// be served over any transport. It exists so cmd/rikugan/main.go can
// stay minimal — log init, signal context, app.Setup, transport — and
// so the per-concept binaries planned for Étape 4 of the refactor
// can share the same wiring layer.
//
// Étape 2 status: imports point at the concept-folder packages
// (internal/<concept>/<provider>) and the pkg/ shared code. Tools
// exposed are the same set as before the refactor, plus AlertManager's
// alertmanager_list_active_alerts.
package app

import (
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/correlation/runes"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	versk8s "github.com/Zuful/rikugan/internal/versioning/kubernetes"
	"github.com/Zuful/rikugan/pkg/dispatcher"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	pkgmcp "github.com/Zuful/rikugan/pkg/mcp"
)

// Setup constructs an MCP server, registers every available provider
// via a dispatcher, and returns the server ready to be wrapped in any
// transport.
//
// Provider availability is environment-driven: Kubernetes is required
// (the cluster client is shared between orchestration/kubernetes and
// versioning/kubernetes — both fail to register if the client can't
// be built); every other provider degrades to warn-and-skip if its
// env config is missing. Prometheus and runes are coupled — runes
// needs both the k8s client and prometheus to be present, so it
// registers only when prometheus is.
func Setup(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := pkgmcp.NewServer("rikugan", version)
	d := dispatcher.New(mcpSrv, logger)

	// ── Kubernetes is required — one client, two providers. ────────
	k8sClient, err := pkgk8s.BuildClient("")
	if err != nil {
		return nil, fmt.Errorf("build k8s client: %w", err)
	}
	if err := d.Register(orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register orchestration/kubernetes provider: %w", err)
	}
	if err := d.Register(versk8s.NewFromClient(k8sClient, versk8s.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register versioning/kubernetes provider: %w", err)
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
