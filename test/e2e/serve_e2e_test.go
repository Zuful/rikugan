//go:build e2e

// Package e2e runs rikugan serve's daemon against a real cluster and a
// real Prometheus: the only end-to-end proof available without a
// production cluster. The Collector has otherwise only met fakes, and the
// PromQL templates are where silent bugs hide (label names, rate windows,
// histogram buckets).
//
// CI sets it up (job "E2E (kind + Prometheus)"); locally:
//
//	kind create cluster --name e2e --config test/e2e/kind.yaml
//	docker build -f test/e2e/app/Dockerfile -t rikugan-e2e-app:test .
//	kind load docker-image rikugan-e2e-app:test --name e2e
//	kubectl apply -f test/e2e/prometheus.yaml
//	go test -tags e2e -v -count=1 -timeout 20m ./test/e2e/
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	deployk8s "github.com/Zuful/rikugan/internal/deployments/kubernetes"
	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/metrics/prometheus"
	orchk8s "github.com/Zuful/rikugan/internal/orchestration/kubernetes"
	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/internal/watch/journal"
	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	image = "rikugan-e2e-app:test"
	// checkpoint replaces +5/+15/+30: one checkpoint, 90 s after
	// completion, with two-minute before windows.
	checkpoint   = 90 * time.Second
	beforeWindow = 2 * time.Minute
	// warmUp lets Prometheus collect more than a before window of the
	// first revision, and keeps the first rollout's start out of the
	// second one's before window.
	warmUp = 3*time.Minute + 30*time.Second
)

// config shrinks the windows to fit a CI job, and turns off what this
// workload cannot exercise: saturation (no cAdvisor scrape) and the
// warm-up latency cap (the checkpoint is shorter than +15).
func config(provider.Service) (health.Config, health.Templates) {
	cfg := health.DefaultConfig()
	cfg.BeforeWindow = beforeWindow
	cfg.StaleAfter = 30 * time.Minute
	cfg.MinRequests = 100
	cfg.LatencyDegradedFromCheckpoint = 0
	cfg.SaturationFromCheckpoint = time.Hour
	return cfg, health.DefaultTemplates()
}

func TestE2E_ServeChecksAndNotifiesRealRollouts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cs := client(t)
	promURL := os.Getenv("PROMETHEUS_E2E_URL")
	if promURL == "" {
		promURL = "http://localhost:30090"
	}
	waitPrometheus(ctx, t, promURL)
	ns := namespace(ctx, t, cs)

	// Three services on their first revision, all healthy.
	for _, name := range []string{"healthy", "errors", "crash", "slow"} {
		if _, err := cs.AppsV1().Deployments(ns).Create(ctx, deployment(name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	source := deployk8s.New(cs, nil).WithNamespaces(ns)
	for _, name := range []string{"healthy", "errors", "crash", "slow"} {
		waitRollout(ctx, t, source, ns, name, "1", provider.RolloutComplete)
	}
	t.Logf("first revisions complete; warming up for %s", warmUp)
	select {
	case <-time.After(warmUp):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	promAPI, err := prometheus.BuildAPI(promURL)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	inbox := &inbox{}
	checks := make(chan watch.Check, 32)
	daemon := &watch.Daemon{
		Source: source,
		Checker: &health.Collector{
			Metrics:       prometheus.NewFromAPI(promAPI),
			Orchestration: orchk8s.NewFromClient(cs),
			Deployments:   source,
			Settings:      config,
		},
		Journal:     j,
		Settings:    func(svc provider.Service) health.Config { c, _ := config(svc); return c },
		Checkpoints: []time.Duration{checkpoint},
		Notifier:    &watch.Notifier{Channels: []provider.CommunicationProvider{inbox}},
		OnCheck:     func(_ context.Context, c watch.Check) { checks <- c },
	}
	runCtx, stop := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- daemon.Run(runCtx) }()
	defer func() {
		stop()
		if err := <-errc; err != nil {
			t.Errorf("daemon: %v", err)
		}
	}()

	// Second revisions: the same, a 50% error rate, and one that crashes
	// at start and never becomes ready.
	setEnv(ctx, t, cs, ns, "healthy", "VERSION", "2")
	setEnv(ctx, t, cs, ns, "errors", "ERROR_RATE", "0.5")
	setEnv(ctx, t, cs, ns, "crash", "CRASH", "true")
	setEnv(ctx, t, cs, ns, "slow", "LATENCY_MS", "300")

	want := map[string]func(watch.Check) error{
		"healthy": func(c watch.Check) error {
			return expect(c, checkpoint, health.StatusOK, nil)
		},
		"errors": func(c watch.Check) error {
			return expect(c, checkpoint, health.StatusDegraded, map[string]health.Status{health.SignalErrorRate: health.StatusDegraded})
		},
		"crash": func(c watch.Check) error {
			return expect(c, watch.FailureCheckpoint, health.StatusDegraded, map[string]health.Status{health.SignalRollout: health.StatusDegraded})
		},
		"slow": func(c watch.Check) error {
			return expect(c, checkpoint, health.StatusDegraded, map[string]health.Status{health.SignalLatency: health.StatusDegraded})
		},
	}
	// The same checks with the median method, logged side by side with
	// the window method the daemon uses.
	median := &health.Collector{
		Metrics:       prometheus.NewFromAPI(promAPI),
		Orchestration: orchk8s.NewFromClient(cs),
		Deployments:   source,
		Settings: func(svc provider.Service) (health.Config, health.Templates) {
			cfg, tpl := config(svc)
			cfg.LatencyMethod = health.LatencyMedian
			return cfg, tpl
		},
	}
	for len(want) > 0 {
		select {
		case c := <-checks:
			name := c.Rollout.Service.Name
			t.Logf("check %s revision %s at %s: %s %s", name, c.Rollout.Sequence, c.Checkpoint, c.Report.Outcome, c.Report.Verdict)
			for _, s := range c.Report.Signals {
				t.Logf("    %s %s: %s", s.Name, s.Status, s.Reason)
			}
			for _, n := range c.Report.Notes {
				t.Logf("    note: %s", n)
			}
			check, pending := want[name]
			if !pending || c.Rollout.Sequence != "2" {
				continue
			}
			if c.Checkpoint != watch.FailureCheckpoint {
				if m, err := median.CheckRollout(ctx, c.Rollout, health.CheckOptions{Checkpoint: c.Checkpoint}); err == nil {
					w, _ := c.Report.Signal(health.SignalLatency)
					med, _ := m.Signal(health.SignalLatency)
					t.Logf("    latency, window method: %s %s", w.Status, w.Reason)
					t.Logf("    latency, median method: %s %s", med.Status, med.Reason)
				}
			}
			if err := check(c); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			delete(want, name)
		case <-ctx.Done():
			t.Fatalf("no second-revision check for %v", keys(want))
		}
	}

	got := inbox.titles()
	t.Logf("notifications:\n%s", strings.Join(got, "\n"))
	for _, wantTitle := range []string{
		fmt.Sprintf("[critical] %s/errors: revision 2 degraded (+%s)", ns, checkpoint),
		fmt.Sprintf("[critical] %s/crash: revision 2 degraded (rollout failure)", ns),
		fmt.Sprintf("[critical] %s/slow: revision 2 degraded (+%s)", ns, checkpoint),
	} {
		if !contains(got, wantTitle) {
			t.Errorf("missing notification %q", wantTitle)
		}
	}
	for _, title := range got {
		if strings.Contains(title, "/healthy:") && strings.Contains(title, "revision 2") {
			t.Errorf("healthy rollout notified: %q", title)
		}
	}
}

func expect(c watch.Check, cp time.Duration, verdict health.Status, signals map[string]health.Status) error {
	if c.Checkpoint != cp || c.Report.Verdict != verdict {
		return fmt.Errorf("checked at %s with verdict %q (%s), want %s at %s", c.Checkpoint, c.Report.Verdict, c.Report.Outcome, verdict, cp)
	}
	for name, want := range signals {
		if s, ok := c.Report.Signal(name); !ok || s.Status != want {
			return fmt.Errorf("signal %s: %+v, want %s", name, s, want)
		}
	}
	return nil
}

// inbox is a notification channel that keeps what it receives.
type inbox struct {
	provider.CommunicationProvider
	mu   sync.Mutex
	msgs []provider.TeamMessage
}

func (i *inbox) Name() string { return "inbox" }

func (i *inbox) PostMessage(_ context.Context, m provider.TeamMessage) (provider.MessageRef, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.msgs = append(i.msgs, m)
	return "", nil
}

func (i *inbox) titles() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []string
	for _, m := range i.msgs {
		out = append(out, fmt.Sprintf("[%s] %s", m.Severity, m.Title))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func keys(m map[string]func(watch.Check) error) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func client(t *testing.T) kubernetes.Interface {
	t.Helper()
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("kubeconfig %s: %v", path, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func waitPrometheus(ctx context.Context, t *testing.T, url string) {
	t.Helper()
	for {
		resp, err := http.Get(url + "/-/ready") // #nosec G107 -- test-only URL
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("prometheus at %s not ready: %v", url, err)
		case <-time.After(2 * time.Second):
		}
	}
}

func namespace(ctx context.Context, t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	ns, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{}) })
	return ns.Name
}

func deployment(name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	replicas := int32(1)
	deadline := int32(60)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas:                &replicas,
			ProgressDeadlineSeconds: &deadline,
			Selector:                &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"prometheus.io/scrape": "true"},
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(int64),
					Containers: []corev1.Container{{
						Name:            "app",
						Image:           image,
						ImagePullPolicy: corev1.PullNever, // loaded into kind by CI
						Ports:           []corev1.ContainerPort{{ContainerPort: 8080}},
						Env:             []corev1.EnvVar{{Name: "VERSION", Value: "1"}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
							PeriodSeconds: 2,
						},
					}},
				},
			},
		},
	}
}

func setEnv(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name, key, value string) {
	t.Helper()
	body := fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":%q,"value":%q}]}]}}}}`, key, value)
	if _, err := cs.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(body), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
}

func waitRollout(ctx context.Context, t *testing.T, src *deployk8s.Provider, ns, name, seq string, state provider.RolloutState) {
	t.Helper()
	for {
		r, err := src.LastRollout(ctx, provider.Service{Name: name, Namespace: ns})
		if err == nil && r.Sequence == seq && r.State == state {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never reached revision %s %s (last %+v, %v)", name, seq, state, r, err)
		case <-time.After(time.Second):
		}
	}
}
