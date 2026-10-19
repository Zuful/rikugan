package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Zuful/rikugan/internal/communication"
	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	"github.com/Zuful/rikugan/internal/watch"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	"github.com/Zuful/rikugan/pkg/provider"
)

// DoctorOptions tunes rikugan doctor.
type DoctorOptions struct {
	// SendTest sends a test message on every notification channel and
	// pings the heartbeat URL: real side effects, so only on request.
	SendTest bool
	Logger   *slog.Logger
}

// accessRule is one permission rikugan serve needs on Kubernetes, as the
// Helm chart's Role grants it.
type accessRule struct{ group, resource, verb string }

var serveAccess = []accessRule{
	{"apps", "deployments", "get"}, {"apps", "deployments", "list"}, {"apps", "deployments", "watch"},
	{"apps", "replicasets", "list"}, {"apps", "replicasets", "watch"},
	{"", "pods", "list"}, {"", "pods", "watch"},
	{"", "events", "list"},
}

// doctorDeps builds what the checks talk to; the tests replace them.
type doctorDeps struct {
	metrics  func() (provider.MetricsProvider, error)
	backend  func(backend string, namespaces []string) (provider.OrchestrationProvider, provider.DeploymentSource, error)
	channels func() ([]provider.CommunicationProvider, error)
	// access reports whether rikugan may do rule in namespace; nil
	// outside Kubernetes.
	access func(ctx context.Context, namespace string, rule accessRule) (bool, error)
	http   *http.Client
	now    func() time.Time
}

func defaultDoctorDeps(logger *slog.Logger) doctorDeps {
	return doctorDeps{
		metrics: func() (provider.MetricsProvider, error) {
			api, err := prometheus.BuildAPI("")
			if err != nil {
				return nil, err
			}
			return prometheus.NewFromAPI(api, prometheus.WithLogger(logger)), nil
		},
		backend: func(b string, ns []string) (provider.OrchestrationProvider, provider.DeploymentSource, error) {
			return serveBackend(b, ns, logger)
		},
		channels: func() ([]provider.CommunicationProvider, error) { return communication.Configured(logger) },
		access: func(ctx context.Context, namespace string, rule accessRule) (bool, error) {
			client, err := pkgk8s.BuildClient("")
			if err != nil {
				return false, err
			}
			review, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{
				Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
					Namespace: namespace, Group: rule.group, Resource: rule.resource, Verb: rule.verb,
				}},
			}, metav1.CreateOptions{})
			if err != nil {
				return false, err
			}
			return review.Status.Allowed, nil
		},
		http: &http.Client{Timeout: 10 * time.Second},
		now:  time.Now,
	}
}

// Doctor checks rikugan serve's configuration end to end, reading the
// environment as serve does: the verdicts API, the state directory, the
// backends, the rights on Kubernetes, every service's rollouts and query
// templates, the notification channels and the heartbeat. It writes a
// checklist to w and reports whether every check passed; warnings do
// not fail it.
func Doctor(ctx context.Context, w io.Writer, opts DoctorOptions) bool {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return runDoctor(ctx, w, opts, defaultDoctorDeps(logger), logger)
}

type checklist struct {
	w      io.Writer
	failed bool
}

func (c *checklist) line(mark, name, msg string, hints ...string) {
	_, _ = fmt.Fprintf(c.w, "%s %-22s %s\n", mark, name, msg)
	for _, h := range hints {
		_, _ = fmt.Fprintf(c.w, "  %-22s → %s\n", "", h)
	}
}

func (c *checklist) ok(name, msg string)                   { c.line("✓", name, msg) }
func (c *checklist) warn(name, msg string, hint ...string) { c.line("!", name, msg, hint...) }
func (c *checklist) skip(name, msg string)                 { c.line("-", name, msg) }
func (c *checklist) fail(name, msg string, hint ...string) {
	c.failed = true
	c.line("✗", name, msg, hint...)
}

func runDoctor(ctx context.Context, w io.Writer, opts DoctorOptions, deps doctorDeps, logger *slog.Logger) bool {
	c := &checklist{w: w}
	_, _ = fmt.Fprintln(w, "rikugan doctor: rikugan serve's configuration, as it would read it")

	// Verdicts API.
	listen := os.Getenv("RIKUGAN_SERVE_LISTEN")
	if listen == "" {
		listen = DefaultServeListen
	}
	if err := watch.CheckListen(listen, os.Getenv("RIKUGAN_SERVE_TOKEN")); err != nil {
		c.fail("verdicts API", err.Error())
	} else if os.Getenv("RIKUGAN_SERVE_TOKEN") != "" {
		c.ok("verdicts API", listen+", token required")
	} else {
		c.ok("verdicts API", listen+", loopback: no token needed")
	}

	// State directory: writable, and whether a daemon holds it.
	stateDir := os.Getenv("RIKUGAN_STATE_DIR")
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	if release, err := watch.Lock(stateDir); err == nil {
		_ = release()
		c.ok("state directory", stateDir+", writable; no rikugan serve running on it")
	} else if strings.Contains(err.Error(), "another rikugan serve") {
		c.ok("state directory", stateDir+", in use by a running rikugan serve")
	} else {
		c.fail("state directory", err.Error(), "RIKUGAN_STATE_DIR must be a local directory rikugan can write")
	}

	// Health settings.
	settings, err := health.LoadSettings(os.Getenv("RIKUGAN_HEALTH_CONFIG"))
	switch {
	case err != nil:
		c.fail("health settings", err.Error())
	case os.Getenv("RIKUGAN_HEALTH_CONFIG") == "":
		c.ok("health settings", "defaults (RIKUGAN_HEALTH_CONFIG not set)")
	default:
		c.ok("health settings", os.Getenv("RIKUGAN_HEALTH_CONFIG")+", parsed")
	}

	// Metrics backend.
	metrics, err := deps.metrics()
	if err != nil {
		c.fail("metrics backend", err.Error(), "rikugan serve needs PROMETHEUS_URL")
		metrics = nil
	} else if _, err := metrics.GetMetric(ctx, "vector(1)", deps.now().Add(-time.Minute), deps.now(), time.Minute); err != nil {
		c.fail("metrics backend", fmt.Sprintf("%s does not answer: %v", metrics.Name(), err))
		metrics = nil
	} else {
		c.ok("metrics backend", metrics.Name()+" answers")
	}

	// Orchestration backend and the watched namespaces.
	backend := os.Getenv("ORCHESTRATION_PROVIDER")
	if backend == "" {
		backend = "kubernetes"
	}
	namespaces, err := serveNamespaces(backend)
	if err != nil {
		c.fail("watched namespaces", err.Error())
		return c.finish(w)
	}
	orch, source, err := deps.backend(backend, namespaces)
	if err != nil {
		c.fail("orchestration", err.Error())
		return c.finish(w)
	}
	var services []provider.Service
	for _, ns := range namespaces {
		list, err := orch.ListServices(ctx, ns)
		if err != nil {
			c.fail("orchestration", fmt.Sprintf("%s: cannot list the services of %s: %v", backend, ns, err))
			continue
		}
		services = append(services, list...)
		noun := "services"
		if len(list) == 1 {
			noun = "service"
		}
		c.ok("orchestration", fmt.Sprintf("%s, %s: %d %s", backend, ns, len(list), noun))
	}

	// Rights on Kubernetes, as the Helm chart's Role grants them.
	if backend == "kubernetes" && deps.access != nil {
		for _, ns := range namespaces {
			var missing []string
			for _, rule := range serveAccess {
				allowed, err := deps.access(ctx, ns, rule)
				if err != nil {
					c.warn("RBAC", fmt.Sprintf("%s: cannot review rights: %v", ns, err))
					missing = nil
					break
				}
				if !allowed {
					missing = append(missing, rule.verb+" "+rule.resource)
				}
			}
			if len(missing) > 0 {
				c.fail("RBAC", fmt.Sprintf("%s: missing %s", ns, strings.Join(missing, ", ")),
					"the Helm chart's Role (serve.enabled) grants get, list and watch on deployments, replicasets, pods and events")
			} else {
				c.ok("RBAC", ns+": rollouts, pods and events readable")
			}
		}
	}

	// Each service: its current rollout, then its query templates.
	collector := &health.Collector{Metrics: metrics, Orchestration: orch, Deployments: source, Settings: settings.For, Now: deps.now}
	for _, svc := range services {
		name := svc.Namespace + "/" + svc.Name
		r, err := source.LastRollout(ctx, svc)
		if err != nil {
			c.warn(name, "no rollout: "+err.Error())
		} else {
			c.ok(name, fmt.Sprintf("revision %s, %s", r.Label(), r.State))
		}
		if metrics == nil {
			c.skip(name, "templates not checked: no metrics backend")
			continue
		}
		probes, err := collector.ProbeTemplates(ctx, svc, time.Hour)
		if err != nil {
			c.fail(name, "templates not checked: "+err.Error())
			continue
		}
		templateLines(c, name, probes)
	}

	// Notification channels.
	channels, err := deps.channels()
	switch {
	case err != nil:
		c.fail("notifications", err.Error())
	case len(channels) == 0:
		c.warn("notifications", "no channel: verdicts are recorded but nobody is told",
			"set TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID, or SLACK_WEBHOOK_URL")
	}
	for _, ch := range channels {
		if !opts.SendTest {
			c.ok("notifications", ch.Name()+" configured (not tried: --send-test sends a message)")
			continue
		}
		err := ch.PostMessage(ctx, provider.TeamMessage{
			Severity: provider.SeverityInfo,
			Title:    "rikugan doctor: test message",
			Body:     "If you read this, rikugan serve can reach this channel.",
		})
		if err != nil {
			c.fail("notifications", ch.Name()+": "+err.Error())
		} else {
			c.ok("notifications", ch.Name()+": test message sent")
		}
	}

	// Alerts consulted before each message.
	if b, ok := primaryAlertsBackend(availableAlertsBackends(logger), logger); ok {
		c.ok("alerts", b.name+", consulted before each message")
	} else {
		c.skip("alerts", "no alerts backend: messages do not list firing alerts")
	}

	// Heartbeat.
	hb, err := serveHeartbeat(logger)
	switch {
	case err != nil:
		c.fail("heartbeat", err.Error())
	case hb == nil:
		c.skip("heartbeat", "RIKUGAN_HEARTBEAT_URL not set: nothing notices if the daemon stops")
	case !opts.SendTest:
		c.ok("heartbeat", fmt.Sprintf("every %s (not tried: --send-test pings it)", hb.Interval))
	default:
		// The URL usually holds a secret: report the status, never the URL.
		resp, err := deps.http.Get(hb.URL)
		if err != nil {
			c.fail("heartbeat", "the ping did not go through")
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				c.fail("heartbeat", fmt.Sprintf("the ping got HTTP %d", resp.StatusCode))
			} else {
				c.ok("heartbeat", fmt.Sprintf("pinged, every %s", hb.Interval))
			}
		}
	}

	// Sweep and Compose settings.
	if sw, err := serveSweep(namespaces, orch, nil); err != nil {
		c.fail("sweep", err.Error())
	} else if sw == nil {
		c.skip("sweep", "off")
	} else {
		interval := sw.Interval
		if interval == 0 {
			interval = watch.DefaultSweepInterval
		}
		c.ok("sweep", fmt.Sprintf("every %s", interval))
	}
	if backend == "dockercompose" {
		if d, err := composeRolloutDeadline(); err != nil {
			c.fail("rollout deadline", err.Error())
		} else {
			c.ok("rollout deadline", d.String())
		}
	}
	return c.finish(w)
}

// templateLines reports a service's template probes: refused ones fail,
// empty or unaggregated ones warn. An empty error_rate is normal: a
// service that served no 5xx has no 5xx series.
func templateLines(c *checklist, name string, probes []health.TemplateProbe) {
	sort.SliceStable(probes, func(i, j int) bool { return probes[i].Name < probes[j].Name })
	var fine []string
	for _, p := range probes {
		switch {
		case p.Invalid:
			c.fail(name, fmt.Sprintf("template %s invalid: %s", p.Name, p.Err), "fix it in RIKUGAN_HEALTH_CONFIG")
		case p.Series == 0 && p.Name != "error_rate":
			c.warn(name, fmt.Sprintf("template %s selects nothing over the last hour", p.Name),
				"check its metric and label names against what the backend has, or the service had no traffic")
		case p.Series > 1:
			c.warn(name, fmt.Sprintf("template %s returns %d series: it must aggregate to one", p.Name, p.Series))
		default:
			fine = append(fine, p.Name)
		}
	}
	if len(fine) > 0 {
		c.ok(name, "templates with data: "+strings.Join(fine, ", "))
	}
}

func (c *checklist) finish(w io.Writer) bool {
	if c.failed {
		_, _ = fmt.Fprintln(w, "\nSome checks failed (✗): rikugan serve would not run, or would run blind.")
		return false
	}
	_, _ = fmt.Fprintln(w, "\nNo check failed.")
	return true
}
