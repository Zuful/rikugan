package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Zuful/rikugan/internal/communication"
	deploydc "github.com/Zuful/rikugan/internal/deployments/dockercompose"
	deployk8s "github.com/Zuful/rikugan/internal/deployments/kubernetes"
	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	"github.com/Zuful/rikugan/internal/orchestration/dockercompose"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/internal/watch/journal"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	"github.com/Zuful/rikugan/pkg/provider"
)

// DefaultServeListen is the verdicts API's address when
// RIKUGAN_SERVE_LISTEN is not set: loopback, where no token is required.
const DefaultServeListen = "127.0.0.1:8080"

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
// The orchestration backend (ORCHESTRATION_PROVIDER: kubernetes or
// dockercompose) also detects the rollouts; a metrics backend is
// required.
func Serve(ctx context.Context, logger, verdicts *slog.Logger) error {
	listen := os.Getenv("RIKUGAN_SERVE_LISTEN")
	if listen == "" {
		listen = DefaultServeListen
	}
	token := os.Getenv("RIKUGAN_SERVE_TOKEN")
	if err := watch.CheckListen(listen, token); err != nil {
		return err
	}

	stateDir := os.Getenv("RIKUGAN_STATE_DIR")
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	release, err := watch.Lock(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	promAPI, err := prometheus.BuildAPI("")
	if err != nil {
		return fmt.Errorf("rikugan serve needs a metrics backend: %w", err)
	}
	backend := os.Getenv("ORCHESTRATION_PROVIDER")
	if backend == "" {
		backend = "kubernetes"
	}
	namespaces, err := serveNamespaces(backend)
	if err != nil {
		return err
	}
	orchestration, source, err := serveBackend(backend, namespaces, logger)
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

	collector := &health.Collector{
		Metrics:       prometheus.NewFromAPI(promAPI, prometheus.WithLogger(logger)),
		Orchestration: orchestration,
		Deployments:   source,
		Settings:      settings.For,
	}
	sweepCfg, err := serveSweep(namespaces, orchestration, collector)
	if err != nil {
		return err
	}
	daemon := &watch.Daemon{
		Source:  source,
		Checker: collector,
		Sweep:   sweepCfg,
		Journal: j,
		Settings: func(svc provider.Service) health.Config {
			cfg, _ := settings.For(svc)
			return cfg
		},
		Notifier: notifier,
		Verdicts: verdicts,
		Logger:   logger,
	}
	if daemon.Heartbeat, err = serveHeartbeat(logger); err != nil {
		return err
	}
	logger.Info("rikugan serve starting",
		slog.String("state_dir", stateDir),
		slog.String("namespaces", strings.Join(namespaces, ",")),
		slog.String("listen", listen),
		slog.Bool("token", token != ""),
		slog.Bool("heartbeat", daemon.Heartbeat != nil),
	)
	return runWithAPI(ctx, daemon, &http.Server{
		Addr:              listen,
		Handler:           watch.NewAPI(j, token, nil),
		ReadHeaderTimeout: 10 * time.Second,
	})
}

// runWithAPI runs the daemon and the verdicts API together: either one
// stopping stops the other.
func runWithAPI(ctx context.Context, daemon *watch.Daemon, srv *http.Server) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	apiErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("verdicts API: %w", err)
		}
		apiErr <- err
		cancel()
	}()
	err := daemon.Run(ctx)
	shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = srv.Shutdown(shutdownCtx)
	if aerr := <-apiErr; aerr != nil {
		return aerr
	}
	return err
}

// serveBackend builds the orchestration provider and the matching
// DeploymentSource for the watched namespaces (Compose projects on
// Docker Compose).
func serveBackend(backend string, namespaces []string, logger *slog.Logger) (provider.OrchestrationProvider, provider.DeploymentSource, error) {
	switch backend {
	case "kubernetes":
		k8sClient, err := pkgk8s.BuildClient("")
		if err != nil {
			return nil, nil, fmt.Errorf("build k8s client: %w", err)
		}
		return orchk8s.NewFromClient(k8sClient, orchk8s.WithLogger(logger)),
			deployk8s.New(k8sClient, logger).WithNamespaces(namespaces...), nil
	case "dockercompose":
		deadline, err := composeRolloutDeadline()
		if err != nil {
			return nil, nil, err
		}
		orch := dockercompose.New(dockercompose.WithLogger(logger), dockercompose.WithProject(namespaces[0]))
		return orch, deploydc.New(orch, deploydc.WithProjects(namespaces...), deploydc.WithRolloutDeadline(deadline), deploydc.WithLogger(logger)), nil
	}
	return nil, nil, fmt.Errorf("ORCHESTRATION_PROVIDER=%q: rikugan serve supports kubernetes and dockercompose", backend)
}

// composeRolloutDeadline reads RIKUGAN_COMPOSE_ROLLOUT_DEADLINE
// (compose_rollout_deadline, default 10m).
func composeRolloutDeadline() (time.Duration, error) {
	raw := os.Getenv("RIKUGAN_COMPOSE_ROLLOUT_DEADLINE")
	if raw == "" {
		return deploydc.DefaultRolloutDeadline, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 10*time.Second {
		return 0, fmt.Errorf("RIKUGAN_COMPOSE_ROLLOUT_DEADLINE: want a duration of at least 10s, such as 10m; got %q", raw)
	}
	return d, nil
}

// serveSweep reads the periodic sweep's settings (design §6):
// RIKUGAN_SWEEP_INTERVAL (default 4h, "off" to disable),
// RIKUGAN_SWEEP_NAMESPACES (default: the watched namespaces),
// RIKUGAN_SWEEP_EXCLUDE (names or namespace/name),
// RIKUGAN_SWEEP_NOTIFY_WARNING and RIKUGAN_SWEEP_WARNING_PERSISTENCE.
func serveSweep(namespaces []string, services provider.OrchestrationProvider, checker watch.SweepChecker) (*watch.SweepConfig, error) {
	cfg := &watch.SweepConfig{Namespaces: namespaces, Services: services, Checker: checker}
	switch raw := os.Getenv("RIKUGAN_SWEEP_INTERVAL"); raw {
	case "":
	case "off":
		return nil, nil
	default:
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Minute {
			return nil, fmt.Errorf("RIKUGAN_SWEEP_INTERVAL: want a duration of at least 1m, such as 4h, or off; got %q", raw)
		}
		cfg.Interval = d
	}
	if ns := splitList(os.Getenv("RIKUGAN_SWEEP_NAMESPACES")); len(ns) > 0 {
		cfg.Namespaces = ns
	}
	cfg.Exclude = splitList(os.Getenv("RIKUGAN_SWEEP_EXCLUDE"))
	cfg.State.NotifyWarning = os.Getenv("RIKUGAN_SWEEP_NOTIFY_WARNING") == "true"
	if raw := os.Getenv("RIKUGAN_SWEEP_WARNING_PERSISTENCE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("RIKUGAN_SWEEP_WARNING_PERSISTENCE: want a positive integer, got %q", raw)
		}
		cfg.State.WarningPersistence = n
	}
	return cfg, nil
}

func splitList(raw string) []string {
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// serveHeartbeat reads RIKUGAN_HEARTBEAT_URL, a dead man's switch the
// daemon pings every RIKUGAN_HEARTBEAT_INTERVAL (default 5m) while it
// runs; nil when unset.
func serveHeartbeat(logger *slog.Logger) (*watch.Heartbeat, error) {
	raw := os.Getenv("RIKUGAN_HEARTBEAT_URL")
	if raw == "" {
		return nil, nil
	}
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		// The URL usually holds a secret: never echo it.
		return nil, errors.New("RIKUGAN_HEARTBEAT_URL: want an http(s) URL")
	}
	hb := &watch.Heartbeat{URL: raw, Interval: watch.DefaultHeartbeatInterval, Logger: logger,
		Client: &http.Client{Timeout: 10 * time.Second}}
	if v := os.Getenv("RIKUGAN_HEARTBEAT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			return nil, fmt.Errorf("RIKUGAN_HEARTBEAT_INTERVAL: want a duration of at least 10s, such as 5m; got %q", v)
		}
		hb.Interval = d
	}
	return hb, nil
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
// namespace rikugan itself runs in; on Docker Compose, the projects, by
// default COMPOSE_PROJECT_NAME. Watching everything would need
// cluster-wide rights, so it is never the silent default.
func serveNamespaces(backend string) ([]string, error) {
	if out := splitList(os.Getenv("RIKUGAN_SERVE_NAMESPACES")); len(out) > 0 {
		return out, nil
	}
	if backend == "dockercompose" {
		if p := os.Getenv("COMPOSE_PROJECT_NAME"); p != "" {
			return []string{p}, nil
		}
		return nil, errors.New("set COMPOSE_PROJECT_NAME (or RIKUGAN_SERVE_NAMESPACES): the Compose project whose services rikugan serve watches")
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
