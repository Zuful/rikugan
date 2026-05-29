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
	"github.com/Zuful/rikugan/internal/logs"
	"github.com/Zuful/rikugan/internal/logs/loki"
	"github.com/Zuful/rikugan/internal/metrics"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	"github.com/Zuful/rikugan/internal/orchestration"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	"github.com/Zuful/rikugan/pkg/dispatcher"
	"github.com/Zuful/rikugan/pkg/provider"
)

// registerOrchestrationConcept registers the kubernetes-backed
// orchestration provider (find_pods_by_service_name) and the concept
// provider (list_services / get_service_state / describe_service /
// get_recent_events / restart_service / get_service_history).
// The k8s client is supplied by the caller because it is also used by
// versioning/kubernetes and by runes — Setup() builds it once.
func registerOrchestrationConcept(d *dispatcher.Dispatcher, k8sClient kubernetes.Interface, logger *slog.Logger) error {
	orchKube := orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger))
	if err := d.Register(orchKube); err != nil {
		return fmt.Errorf("register orchestration/kubernetes provider: %w", err)
	}
	if err := d.Register(orchestration.NewConceptProvider(orchKube, "kubernetes", logger)); err != nil {
		return fmt.Errorf("register orchestration concept provider: %w", err)
	}
	return nil
}

// registerMetricsConcept builds the prometheus API and registers the
// metrics concept provider (get_metric / compare_to_baseline /
// list_available_metrics). On missing PROMETHEUS_URL the call is
// warn-and-skip and returns a nil promv1.API so the caller (runes) can
// skip itself too.
func registerMetricsConcept(d *dispatcher.Dispatcher, logger *slog.Logger) (promv1.API, error) {
	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		logger.Warn("metrics concept (prometheus) not available, skipping",
			slog.String("reason", err.Error()),
		)
		return nil, nil
	}
	promProvider := prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger))
	if err := d.Register(metrics.NewConceptProvider(promProvider, "prometheus", logger)); err != nil {
		return nil, fmt.Errorf("register metrics concept provider: %w", err)
	}
	return promAPI, nil
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
