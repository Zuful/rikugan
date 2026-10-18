package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Zuful/rikugan/internal/communication"
	deployk8s "github.com/Zuful/rikugan/internal/deployments/kubernetes"
	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/internal/watch/journal"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	"github.com/Zuful/rikugan/pkg/provider"
)

// DefaultStateDir is where rikugan serve keeps its journal and lock when
// RIKUGAN_STATE_DIR is not set; it matches systemd's
// StateDirectory=rikugan.
const DefaultStateDir = "/var/lib/rikugan"

// serviceAccountNamespace holds the namespace of the pod rikugan runs in.
const serviceAccountNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace" // #nosec G101 -- a well-known file path, not a credential

// Serve runs the proactive mode's daemon (rikugan serve) until ctx is
// done: it follows the rollouts of the watched namespaces and checks
// each one at its checkpoints, recording every verdict in the journal
// under the state directory. verdicts receives one structured line per
// check, the copy log pipelines read.
//
// Rollout detection is Kubernetes-only until the Docker Compose
// DeploymentSource lands, and a metrics backend is required.
func Serve(ctx context.Context, logger, verdicts *slog.Logger) error {
	stateDir := os.Getenv("RIKUGAN_STATE_DIR")
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	release, err := watch.Lock(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	if backend := os.Getenv("ORCHESTRATION_PROVIDER"); backend != "" && backend != "kubernetes" {
		return fmt.Errorf("rikugan serve detects rollouts on kubernetes only for now; ORCHESTRATION_PROVIDER is %q", backend)
	}
	k8sClient, err := pkgk8s.BuildClient("")
	if err != nil {
		return fmt.Errorf("build k8s client: %w", err)
	}
	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		return fmt.Errorf("rikugan serve needs a metrics backend: %w", err)
	}
	namespaces, err := serveNamespaces()
	if err != nil {
		return err
	}
	settings, err := health.LoadSettings(os.Getenv("RIKUGAN_HEALTH_CONFIG"))
	if err != nil {
		return err
	}

	notifier, err := serveNotifier(logger)
	if err != nil {
		return err
	}

	j, err := journal.Open(stateDir, journal.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()

	source := deployk8s.New(k8sClient, logger).WithNamespaces(namespaces...)
	daemon := &watch.Daemon{
		Source: source,
		Checker: &health.Collector{
			Metrics:       prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger)),
			Orchestration: orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger)),
			Deployments:   source,
			Settings:      settings.For,
		},
		Journal: j,
		Settings: func(svc provider.Service) health.Config {
			cfg, _ := settings.For(svc)
			return cfg
		},
		Notifier: notifier,
		Verdicts: verdicts,
		Logger:   logger,
	}
	logger.Info("rikugan serve starting",
		slog.String("state_dir", stateDir),
		slog.String("namespaces", strings.Join(namespaces, ",")),
	)
	return daemon.Run(ctx)
}

// serveNotifier builds the notification side: every configured channel
// (a half-configured one stops the daemon rather than going silent), the
// primary alerts backend if any, consulted before each message, and
// RIKUGAN_NOTIFY_ON_OK.
func serveNotifier(logger *slog.Logger) (*watch.Notifier, error) {
	channels, err := communication.Configured(logger)
	if err != nil {
		return nil, fmt.Errorf("notification channels: %w", err)
	}
	if len(channels) == 0 {
		logger.Warn("no notification channel configured: verdicts are recorded but nobody is told",
			slog.String("hint", "set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID, or SLACK_WEBHOOK_URL"))
	}
	n := &watch.Notifier{
		Channels:   channels,
		NotifyOnOK: os.Getenv("RIKUGAN_NOTIFY_ON_OK") == "true",
		Logger:     logger,
		Retries:    3,
		Backoff:    5 * time.Second,
	}
	if b, ok := primaryAlertsBackend(availableAlertsBackends(logger), logger); ok {
		n.Alerts = b.impl
	}
	return n, nil
}

// serveNamespaces returns the namespaces whose rollouts are watched:
// RIKUGAN_SERVE_NAMESPACES (comma-separated) or, when unset, the
// namespace rikugan itself runs in. Watching everything would need
// cluster-wide rights, so it is never the silent default.
func serveNamespaces() ([]string, error) {
	if raw := os.Getenv("RIKUGAN_SERVE_NAMESPACES"); raw != "" {
		var out []string
		for _, ns := range strings.Split(raw, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				out = append(out, ns)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	raw, err := os.ReadFile(serviceAccountNamespace)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("set RIKUGAN_SERVE_NAMESPACES: rikugan serve is not running in a pod, so there is no namespace of its own to watch")
		}
		return nil, fmt.Errorf("read the pod's namespace: %w", err)
	}
	return []string{strings.TrimSpace(string(raw))}, nil
}
