// Package app wires the rikugan MCP server end-to-end: it constructs
// the dispatcher, builds every provider, and returns a server ready to
// be served over any transport. It exists so cmd/rikugan/main.go can
// stay minimal — log init, signal context, app.Setup, transport — and
// so the per-concept binaries planned for Étape 4 of the refactor
// can share the same wiring layer.
//
// Étape 3 status: providers are organised by concept and route through
// the per-concept interfaces (LogsProvider, MetricsProvider,
// AlertsProvider, OrchestrationProvider). Backends still register
// directly when they expose tools that don't project into the strict
// intersection (loki's detect_log_anomalies, sentry's
// list_issues_for_release, pagerduty's get_current_oncalls,
// kubernetes's find_pods_by_service_name). The concept providers in
// internal/<concept>/ expose the canonical tool names. See
// MIGRATION.md for the old → new tool mapping.
package app

import (
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/server"

	"os"

	"github.com/Zuful/rikugan/internal/alerts"
	"github.com/Zuful/rikugan/internal/alerts/alertmanager"
	"github.com/Zuful/rikugan/internal/alerts/pagerduty"
	"github.com/Zuful/rikugan/internal/alerts/sentry"
	"github.com/Zuful/rikugan/internal/correlation/runes"
	"github.com/Zuful/rikugan/internal/logs"
	"github.com/Zuful/rikugan/internal/logs/loki"
	"github.com/Zuful/rikugan/internal/metrics"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	"github.com/Zuful/rikugan/internal/orchestration"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	versk8s "github.com/Zuful/rikugan/internal/versioning/kubernetes"
	"github.com/Zuful/rikugan/pkg/dispatcher"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	pkgmcp "github.com/Zuful/rikugan/pkg/mcp"
	"github.com/Zuful/rikugan/pkg/provider"
)

// Setup constructs an MCP server, registers every available provider
// via a dispatcher, and returns the server ready to be wrapped in any
// transport.
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
	//    1. orchk8s.Provider for its k8s-specific helpers
	//       (find_pods_by_service_name)
	//    2. orchestration.ConceptProvider wrapping orchk8s.Provider as
	//       the OrchestrationProvider impl — exposes the canonical
	//       list_services / get_service_state / describe_service /
	//       get_recent_events / restart_service / get_service_history
	//    3. versk8s.Provider for the versioning concept
	//       (get_image_provenance). All three share the same client.
	k8sClient, err := pkgk8s.BuildClient("")
	if err != nil {
		return nil, fmt.Errorf("build k8s client: %w", err)
	}
	orchKube := orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger))
	if err := d.Register(orchKube); err != nil {
		return nil, fmt.Errorf("register orchestration/kubernetes provider: %w", err)
	}
	if err := d.Register(orchestration.NewConceptProvider(orchKube, "kubernetes", logger)); err != nil {
		return nil, fmt.Errorf("register orchestration concept provider: %w", err)
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

	// ── Logs concept: select a backend, then register both the
	//    backend (for its non-canonical tools, e.g. detect_log_anomalies)
	//    and the concept-level ConceptProvider that exposes the
	//    canonical get_logs / tail_logs / search_logs tools through the
	//    LogsProvider interface. Today only loki is implemented; future
	//    LOGS_PROVIDER values (kubectl, docker-logs) will branch here.
	if err := registerLogsConcept(d, logger); err != nil {
		return nil, err
	}

	// ── Alerts concept: register every available backend's non-canonical
	//    tools (sentry's list_issues_for_release, pagerduty's
	//    get_current_oncalls; alertmanager has none), then select one
	//    as the primary that serves the canonical list_active_alerts /
	//    get_alert_details concept tools. Selection order: ALERTS_PROVIDER
	//    env var if set, else priority alertmanager > sentry > pagerduty.
	if err := registerAlertsConcept(d, logger); err != nil {
		return nil, err
	}

	// ── Metrics concept + Runes: coupled — runes needs both k8s and
	//    prom. The Metrics concept provider wraps prometheus.Provider
	//    and exposes the canonical get_metric / compare_to_baseline /
	//    list_available_metrics tools. The prometheus.Provider itself
	//    is no longer registered directly: it has no backend-specific
	//    tools that don't project into the MetricsProvider interface.
	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		logger.Warn("metrics concept (prometheus) not available, skipping",
			slog.String("reason", err.Error()),
		)
		return mcpSrv, nil
	}
	promProvider := prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger))
	if err := d.Register(metrics.NewConceptProvider(promProvider, "prometheus", logger)); err != nil {
		return nil, fmt.Errorf("register metrics concept provider: %w", err)
	}
	if err := d.Register(runes.New(k8sClient, promAPI, runes.WithLogger(logger))); err != nil {
		return nil, fmt.Errorf("register runes provider: %w", err)
	}

	return mcpSrv, nil
}

// registerAlertsConcept builds every available alerts backend, registers
// each one's backend-specific tools (when it has any), and selects one
// as the primary that the concept-level alerts.ConceptProvider routes
// through. Selection is driven by the ALERTS_PROVIDER env var; when
// unset, the first available backend in priority order
// (alertmanager > sentry > pagerduty) is chosen. A backend that fails
// to build (missing env config) is warned-and-skipped, matching the
// rest of optional providers; dispatcher registration errors are fatal.
func registerAlertsConcept(d *dispatcher.Dispatcher, logger *slog.Logger) error {
	type backend struct {
		name string
		impl provider.AlertsProvider
	}
	var available []backend

	if c, err := alertmanager.BuildClient(""); err == nil {
		p := alertmanager.NewFromClient(c, alertmanager.WithLogger(logger))
		available = append(available, backend{name: "alertmanager", impl: p})
	} else {
		logger.Warn("alertmanager backend not available, skipping", slog.String("reason", err.Error()))
	}
	if c, err := sentry.BuildClient("", "", ""); err == nil {
		p := sentry.NewFromClient(c, sentry.WithLogger(logger))
		available = append(available, backend{name: "sentry", impl: p})
		if err := d.Register(p); err != nil {
			return fmt.Errorf("register sentry provider: %w", err)
		}
	} else {
		logger.Warn("sentry backend not available, skipping", slog.String("reason", err.Error()))
	}
	if c, err := pagerduty.BuildClient(""); err == nil {
		p := pagerduty.NewFromClient(c, pagerduty.WithLogger(logger))
		available = append(available, backend{name: "pagerduty", impl: p})
		if err := d.Register(p); err != nil {
			return fmt.Errorf("register pagerduty provider: %w", err)
		}
	} else {
		logger.Warn("pagerduty backend not available, skipping", slog.String("reason", err.Error()))
	}

	if len(available) == 0 {
		logger.Warn("alerts concept: no backend available, skipping")
		return nil
	}

	requested := os.Getenv("ALERTS_PROVIDER")
	var chosen backend
	if requested != "" {
		for _, b := range available {
			if b.name == requested {
				chosen = b
				break
			}
		}
		if chosen.impl == nil {
			logger.Warn("ALERTS_PROVIDER not available, falling back to first available",
				slog.String("requested", requested),
				slog.String("fallback", available[0].name),
			)
			chosen = available[0]
		}
	} else {
		chosen = available[0]
	}

	logger.Info("alerts concept primary selected",
		slog.String("backend", chosen.name),
	)
	if err := d.Register(alerts.NewConceptProvider(chosen.impl, chosen.name, logger)); err != nil {
		return fmt.Errorf("register alerts concept provider: %w", err)
	}
	return nil
}

// registerLogsConcept builds the operator-selected Logs backend and
// registers it twice on the dispatcher: once as the backend provider
// (so its non-canonical tools like detect_log_anomalies stay exposed),
// once wrapped in a logs.ConceptProvider (which exposes the canonical
// get_logs / tail_logs / search_logs tools routed through the
// LogsProvider interface). A missing-config error from the backend is
// non-fatal and is warned about, matching the rest of the optional
// providers; a dispatcher registration error is fatal.
func registerLogsConcept(d *dispatcher.Dispatcher, logger *slog.Logger) error {
	client, err := loki.BuildClient("")
	if err != nil {
		logger.Warn("logs concept (loki) not available, skipping",
			slog.String("reason", err.Error()),
		)
		return nil
	}
	lokiProvider := loki.NewFromClient(client, loki.WithLogger(logger))
	if err := d.Register(lokiProvider); err != nil {
		return fmt.Errorf("register loki provider: %w", err)
	}
	if err := d.Register(logs.NewConceptProvider(lokiProvider, "loki", logger)); err != nil {
		return fmt.Errorf("register logs concept provider: %w", err)
	}
	return nil
}
