// Package app wires the rikugan MCP server end-to-end: it constructs
// the dispatcher, builds every provider, and returns a server ready to
// be served over any transport.
//
// Étape 4 adds per-concept entry points alongside the unified one:
//
//   - Setup(logger, version)              → unified binary (cmd/rikugan)
//   - SetupOrchestration(logger, version) → cmd/orchestration-mcp
//   - SetupLogs(logger, version)          → cmd/logs-mcp
//   - SetupMetrics(logger, version)       → cmd/metrics-mcp
//   - SetupAlerts(logger, version)        → cmd/alerts-mcp
//
// The per-concept Setups share the helpers in concepts.go so each
// binary only carries the providers it actually needs. The unified
// Setup() also calls these helpers; tools registered through it are
// strictly the union of what the per-concept binaries expose, plus the
// providers that don't belong to a single concept yet (versioning,
// tracing, security, correlation).
package app

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/mark3labs/mcp-go/server"
	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/internal/correlation/runes"
	versk8s "github.com/Zuful/rikugan/internal/versioning/kubernetes"
	"github.com/Zuful/rikugan/pkg/dispatcher"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	pkgmcp "github.com/Zuful/rikugan/pkg/mcp"
)

// Setup constructs an MCP server that registers every available
// provider via a dispatcher. This is the unified binary's entry point.
//
// Provider availability is environment-driven: Kubernetes is required
// (the cluster client is shared between orchestration/kubernetes,
// orchestration concept, and versioning/kubernetes); every other
// provider degrades to warn-and-skip if its env config is missing.
// The metrics concept and runes are coupled — runes needs both the
// k8s client and prometheus to be present.
func Setup(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := pkgmcp.NewServer("rikugan", version)
	d := dispatcher.New(mcpSrv, logger)

	// ── Kubernetes is required — one client, three registrations:
	//    orchestration concept (+ k8s-backed find_pods_by_service_name),
	//    versioning/kubernetes (get_image_provenance), and runes (below).
	k8sClient, err := pkgk8s.BuildClient("")
	if err != nil {
		return nil, fmt.Errorf("build k8s client: %w", err)
	}
	orch, err := registerOrchestrationConcept(d, k8sClient, logger)
	if err != nil {
		return nil, err
	}
	if err := d.Register(versk8s.NewFromClient(k8sClient, versk8s.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register versioning/kubernetes provider: %w", err)
	}

	// ── Optional providers without a concept interface yet: warn-and-skip. ─
	for _, opt := range optionalProviders(logger) {
		if err := registerOptional(d, logger, opt.name, opt.setup); err != nil {
			return nil, err
		}
	}

	// ── Concept providers: each helper warn-and-skips when its backend
	//    config is missing, so the unified binary degrades gracefully.
	if err := registerLogsConcept(d, logger); err != nil {
		return nil, err
	}
	if err := registerAlertsConcept(d, logger); err != nil {
		return nil, err
	}
	promAPI, metricsProvider, err := registerMetricsConcept(d, logger)
	if err != nil {
		return nil, err
	}

	// ── Runes (correlation): six_eyes needs both k8s and prometheus.
	//    Lives only in the unified binary; no per-concept correlation
	//    binary in étape 4 since runes is a consumer that crosses
	//    concept boundaries by design.
	if promAPI != nil {
		if err := d.Register(runes.New(k8sClient, promAPI, runes.WithLogger(logger))); err != nil {
			return nil, fmt.Errorf("register runes provider: %w", err)
		}
	}

	// ── Deploy health (correlation): check_deploy_health reads the
	//    deployments, orchestration and metrics concepts. The only
	//    DeploymentSource today is Kubernetes, so it needs the
	//    kubernetes orchestration backend and a metrics backend.
	if err := registerDeployHealth(d, k8sClient, orch, metricsProvider, logger); err != nil {
		return nil, err
	}

	return mcpSrv, nil
}

// SetupOrchestration constructs an MCP server that registers only the
// orchestration concept. The backend is selected by
// ORCHESTRATION_PROVIDER: when set to "dockercompose" the kubernetes
// client is not built (so the operator does not need a kubeconfig to
// run a docker-compose-only orchestration MCP). Any other value
// (including the default "kubernetes") triggers the usual k8s client
// build; failure to build is fatal in that mode.
//
// Used by cmd/orchestration-mcp.
func SetupOrchestration(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := pkgmcp.NewServer("rikugan-orchestration", version)
	d := dispatcher.New(mcpSrv, logger)

	var k8sClient kubernetes.Interface
	if os.Getenv("ORCHESTRATION_PROVIDER") != "dockercompose" {
		c, err := pkgk8s.BuildClient("")
		if err != nil {
			return nil, fmt.Errorf("build k8s client: %w", err)
		}
		k8sClient = c
	}
	if _, err := registerOrchestrationConcept(d, k8sClient, logger); err != nil {
		return nil, err
	}
	return mcpSrv, nil
}

// SetupLogs constructs an MCP server that registers only the logs
// concept (concept tools + loki's detect_log_anomalies). Used by
// cmd/logs-mcp.
func SetupLogs(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := pkgmcp.NewServer("rikugan-logs", version)
	d := dispatcher.New(mcpSrv, logger)

	if err := registerLogsConcept(d, logger); err != nil {
		return nil, err
	}
	return mcpSrv, nil
}

// SetupMetrics constructs an MCP server that registers only the metrics
// concept (get_metric / compare_to_baseline / list_available_metrics).
// Used by cmd/metrics-mcp. The runes correlation tool is intentionally
// excluded — it depends on both k8s and prometheus and lives only in
// the unified binary.
func SetupMetrics(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := pkgmcp.NewServer("rikugan-metrics", version)
	d := dispatcher.New(mcpSrv, logger)

	if _, _, err := registerMetricsConcept(d, logger); err != nil {
		return nil, err
	}
	return mcpSrv, nil
}

// SetupAlerts constructs an MCP server that registers only the alerts
// concept (list_active_alerts / get_alert_details, plus sentry's
// list_issues_for_release and pagerduty's get_current_oncalls when
// those backends are configured). Used by cmd/alerts-mcp.
func SetupAlerts(logger *slog.Logger, version string) (*server.MCPServer, error) {
	mcpSrv := pkgmcp.NewServer("rikugan-alerts", version)
	d := dispatcher.New(mcpSrv, logger)

	if err := registerAlertsConcept(d, logger); err != nil {
		return nil, err
	}
	return mcpSrv, nil
}
