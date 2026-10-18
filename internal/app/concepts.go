package app

import (
	"fmt"
	"log/slog"
	"os"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/internal/alerts"
	"github.com/Zuful/rikugan/internal/alerts/alertmanager"
	"github.com/Zuful/rikugan/internal/alerts/pagerduty"
	"github.com/Zuful/rikugan/internal/alerts/sentry"
	"github.com/Zuful/rikugan/internal/correlation/deployhealth"
	deployk8s "github.com/Zuful/rikugan/internal/deployments/kubernetes"
	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/logs"
	"github.com/Zuful/rikugan/internal/logs/loki"
	"github.com/Zuful/rikugan/internal/metrics"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	"github.com/Zuful/rikugan/internal/orchestration"
	"github.com/Zuful/rikugan/internal/orchestration/dockercompose"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	"github.com/Zuful/rikugan/pkg/dispatcher"
	"github.com/Zuful/rikugan/pkg/provider"
)

// registerOrchestrationConcept registers the orchestration backend
// selected by the ORCHESTRATION_PROVIDER env var, then wraps it in the
// concept provider that exposes list_services / get_service_state /
// describe_service / get_recent_events / restart_service /
// get_service_history.
//
// Supported values:
//   - kubernetes (default) — uses the k8sClient passed in by the
//     caller (also shared with versioning/kubernetes and runes in
//     the unified binary). Also registers the orchk8s.Provider
//     directly for its find_pods_by_service_name discovery helper.
//   - dockercompose — builds a dockercompose.Provider, picking up
//     COMPOSE_PROJECT_NAME if set. The k8sClient is unused in this
//     mode and may be nil.
//
// Unknown values warn-and-fall-back to kubernetes, mirroring how
// ALERTS_PROVIDER handles unknown backends.
//
// Why k8sClient stays in the signature instead of moving the
// selection up into Setup/SetupOrchestration: the unified Setup()
// also needs the k8sClient for versioning/kubernetes and runes, so
// it builds it unconditionally; piping it through this helper keeps
// one branch point. The per-concept SetupOrchestration() is what
// reads the env var first and decides whether to build the k8s
// client at all (nil-safe contract on dockercompose). The guard
// below catches a misuse where kubernetes is selected without a
// client.
func registerOrchestrationConcept(d *dispatcher.Dispatcher, k8sClient kubernetes.Interface, logger *slog.Logger) (provider.OrchestrationProvider, error) {
	backend := os.Getenv("ORCHESTRATION_PROVIDER")
	if backend == "" {
		backend = "kubernetes"
	}
	switch backend {
	case "kubernetes":
		if k8sClient == nil {
			return nil, fmt.Errorf("ORCHESTRATION_PROVIDER=kubernetes requires a kubernetes client (check KUBECONFIG)")
		}
		orchKube := orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger))
		if err := d.Register(orchKube); err != nil {
			return nil, fmt.Errorf("register orchestration/kubernetes provider: %w", err)
		}
		if err := d.Register(orchestration.NewConceptProvider(orchKube, "kubernetes", logger)); err != nil {
			return nil, fmt.Errorf("register orchestration concept provider: %w", err)
		}
		return orchKube, nil
	case "dockercompose":
		opts := []dockercompose.Option{dockercompose.WithLogger(logger)}
		if project := os.Getenv("COMPOSE_PROJECT_NAME"); project != "" {
			opts = append(opts, dockercompose.WithProject(project))
		}
		dcProvider := dockercompose.New(opts...)
		if err := d.Register(dcProvider); err != nil {
			return nil, fmt.Errorf("register orchestration/dockercompose provider: %w", err)
		}
		if err := d.Register(orchestration.NewConceptProvider(dcProvider, "dockercompose", logger)); err != nil {
			return nil, fmt.Errorf("register orchestration concept provider: %w", err)
		}
		return dcProvider, nil
	default:
		logger.Warn("ORCHESTRATION_PROVIDER unknown, falling back to kubernetes",
			slog.String("requested", backend),
		)
		if k8sClient == nil {
			return nil, fmt.Errorf("ORCHESTRATION_PROVIDER=%q is unknown and the kubernetes fallback has no client", backend)
		}
		orchKube := orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger))
		if err := d.Register(orchKube); err != nil {
			return nil, fmt.Errorf("register orchestration/kubernetes provider: %w", err)
		}
		if err := d.Register(orchestration.NewConceptProvider(orchKube, "kubernetes", logger)); err != nil {
			return nil, fmt.Errorf("register orchestration concept provider: %w", err)
		}
		return orchKube, nil
	}
}

// registerMetricsConcept builds the prometheus API and registers the
// metrics concept provider (get_metric / compare_to_baseline /
// list_available_metrics). On missing PROMETHEUS_URL the call is
// warn-and-skip and returns a nil promv1.API so the caller (runes) can
// skip itself too.
func registerMetricsConcept(d *dispatcher.Dispatcher, logger *slog.Logger) (promv1.API, provider.MetricsProvider, error) {
	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		logger.Warn("metrics concept (prometheus) not available, skipping",
			slog.String("reason", err.Error()),
		)
		return nil, nil, nil
	}
	promProvider := prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger))
	if err := d.Register(metrics.NewConceptProvider(promProvider, "prometheus", logger)); err != nil {
		return nil, nil, fmt.Errorf("register metrics concept provider: %w", err)
	}
	return promAPI, promProvider, nil
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

// registerDeployHealth registers check_deploy_health when its three
// concepts are available: a Kubernetes DeploymentSource (the only one
// so far, so the orchestration backend must be kubernetes too — the
// instances it reads come from the same cluster) and a metrics backend.
// Thresholds and query templates can be overridden per service with the
// YAML file named by RIKUGAN_HEALTH_CONFIG.
func registerDeployHealth(d *dispatcher.Dispatcher, k8sClient kubernetes.Interface, orch provider.OrchestrationProvider, metricsProvider provider.MetricsProvider, logger *slog.Logger) error {
	if metricsProvider == nil || k8sClient == nil {
		logger.Warn("check_deploy_health not available, skipping",
			slog.String("reason", "needs a metrics backend and a kubernetes cluster"),
		)
		return nil
	}
	if _, ok := orch.(*orchk8s.Provider); !ok {
		logger.Warn("check_deploy_health not available, skipping",
			slog.String("reason", "rollout detection is kubernetes-only for now; ORCHESTRATION_PROVIDER is not kubernetes"),
		)
		return nil
	}
	settings, err := health.LoadSettings(os.Getenv("RIKUGAN_HEALTH_CONFIG"))
	if err != nil {
		return err
	}
	collector := &health.Collector{
		Metrics:       metricsProvider,
		Orchestration: orch,
		Deployments:   deployk8s.New(k8sClient, logger),
		Settings:      settings.For,
	}
	if err := d.Register(deployhealth.New(collector, logger)); err != nil {
		return fmt.Errorf("register deploy health provider: %w", err)
	}
	return nil
}
