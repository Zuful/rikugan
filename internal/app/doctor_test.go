package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

type doctorMetrics struct {
	provider.MetricsProvider
	down bool
	// series per query substring; refused queries fail as invalid.
	series  map[string]int
	refused string
}

func (m *doctorMetrics) Name() string { return "prometheus" }

func (m *doctorMetrics) GetMetric(_ context.Context, query string, start, end time.Time, _ time.Duration) ([]provider.MetricSample, error) {
	if m.down {
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	if m.refused != "" && strings.Contains(query, m.refused) {
		return nil, fmt.Errorf("%w: bad_data: parse error", provider.ErrInvalidQuery)
	}
	n := 1
	for k, v := range m.series {
		if strings.Contains(query, k) {
			n = v
		}
	}
	var out []provider.MetricSample
	for i := range n {
		out = append(out, provider.MetricSample{Time: end, Value: 1, Labels: map[string]string{"i": fmt.Sprint(i)}})
	}
	return out, nil
}

type doctorOrch struct {
	provider.OrchestrationProvider
	services []provider.Service
}

func (o *doctorOrch) ListServices(_ context.Context, ns string) ([]provider.Service, error) {
	var out []provider.Service
	for _, s := range o.services {
		if s.Namespace == ns {
			out = append(out, s)
		}
	}
	return out, nil
}

type doctorSource struct{ provider.DeploymentSource }

func (doctorSource) LastRollout(_ context.Context, svc provider.Service) (provider.Rollout, error) {
	if svc.Name == "batch" {
		return provider.Rollout{}, fmt.Errorf("no deployment named batch")
	}
	return provider.Rollout{Service: svc, Sequence: "7", State: provider.RolloutComplete}, nil
}

type doctorChannel struct {
	provider.CommunicationProvider
	sent []provider.TeamMessage
	err  error
}

func (c *doctorChannel) Name() string { return "telegram" }
func (c *doctorChannel) PostMessage(_ context.Context, m provider.TeamMessage) (provider.MessageRef, error) {
	c.sent = append(c.sent, m)
	return "", c.err
}

func doctorEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"RIKUGAN_SERVE_TOKEN", "RIKUGAN_HEALTH_CONFIG", "RIKUGAN_HEARTBEAT_URL", "RIKUGAN_HEARTBEAT_INTERVAL",
		"RIKUGAN_SWEEP_INTERVAL", "ALERTMANAGER_URL", "SENTRY_AUTH_TOKEN", "PAGERDUTY_API_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("ORCHESTRATION_PROVIDER", "kubernetes")
	t.Setenv("RIKUGAN_SERVE_NAMESPACES", "shop")
	t.Setenv("RIKUGAN_SERVE_LISTEN", "127.0.0.1:8080")
	t.Setenv("RIKUGAN_STATE_DIR", t.TempDir())
}

func doctorTestDeps(m *doctorMetrics, ch *doctorChannel, denied string) doctorDeps {
	orch := &doctorOrch{services: []provider.Service{{Namespace: "shop", Name: "api"}, {Namespace: "shop", Name: "batch"}}}
	return doctorDeps{
		metrics: func() (provider.MetricsProvider, error) { return m, nil },
		backend: func(string, []string) (provider.OrchestrationProvider, provider.DeploymentSource, error) {
			return orch, doctorSource{}, nil
		},
		channels: func() ([]provider.CommunicationProvider, error) { return []provider.CommunicationProvider{ch}, nil },
		access: func(_ context.Context, _ string, r accessRule) (bool, error) {
			return r.verb+" "+r.resource != denied, nil
		},
		http: http.DefaultClient,
		now:  func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// has reports whether text contains want, the column padding aside.
func has(text, want string) bool {
	squash := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return strings.Contains(squash(text), squash(want))
}

// Everything that can be wrong at once: each shows, with its hint.
func TestDoctorReportsWhatIsWrong(t *testing.T) {
	doctorEnv(t)
	t.Setenv("RIKUGAN_SERVE_LISTEN", "0.0.0.0:8080") // no token
	var pings atomic.Int32
	hb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pings.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hb.Close()
	t.Setenv("RIKUGAN_HEARTBEAT_URL", hb.URL+"/secret-uuid")

	m := &doctorMetrics{refused: `code=~"5.."`, series: map[string]int{"http_request_duration_seconds": 0, "container_cpu_usage": 3}}
	ch := &doctorChannel{}
	var out bytes.Buffer
	ok := runDoctor(context.Background(), &out, DoctorOptions{SendTest: true}, doctorTestDeps(m, ch, "watch pods"), discard())
	text := out.String()
	if ok {
		t.Errorf("passed:\n%s", text)
	}
	for _, want := range []string{
		"✗ verdicts API",
		"✓ metrics backend         prometheus answers",
		"✓ orchestration           kubernetes, shop: 2 services",
		"✗ RBAC                    shop: missing watch pods",
		"✓ shop/api                revision 7, complete",
		"✗ shop/api                template error_rate invalid: invalid query: bad_data: parse error",
		"! shop/api                template latency_p95 selects nothing over the last hour",
		"! shop/api                template latency_window_p95 selects nothing over the last hour",
		"! shop/api                template cpu returns 3 series: it must aggregate to one",
		"✓ shop/api                templates with data: memory, request_rate",
		"! shop/batch              no rollout: no deployment named batch",
		"✓ notifications           telegram: test message sent",
		"✗ heartbeat               the ping got HTTP 404",
		"Some checks failed",
	} {
		if !has(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret-uuid") {
		t.Errorf("the heartbeat URL leaked:\n%s", text)
	}
	if len(ch.sent) != 1 || pings.Load() != 1 {
		t.Errorf("sent %d messages and %d pings, want one each", len(ch.sent), pings.Load())
	}
}

// A sound configuration passes, and without --send-test nothing is sent.
func TestDoctorPassesWithoutSideEffects(t *testing.T) {
	doctorEnv(t)
	var pings atomic.Int32
	hb := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { pings.Add(1) }))
	defer hb.Close()
	t.Setenv("RIKUGAN_HEARTBEAT_URL", hb.URL)
	ch := &doctorChannel{}
	var out bytes.Buffer
	if !runDoctor(context.Background(), &out, DoctorOptions{}, doctorTestDeps(&doctorMetrics{}, ch, ""), discard()) {
		t.Errorf("failed:\n%s", out.String())
	}
	for _, want := range []string{
		"✓ verdicts API            127.0.0.1:8080, loopback: no token needed",
		"✓ state directory",
		"no rikugan serve running on it",
		"✓ RBAC                    shop: rollouts, pods and events readable",
		"✓ notifications           telegram configured (not tried: --send-test sends a message)",
		"✓ heartbeat               every 5m0s (not tried: --send-test pings it)",
		"✓ sweep                   every 4h0m0s",
		"No check failed.",
	} {
		if !has(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if len(ch.sent) != 0 || pings.Load() != 0 {
		t.Errorf("side effects without --send-test: %d messages, %d pings", len(ch.sent), pings.Load())
	}
}

// The metrics backend down: said once, and the templates are skipped
// rather than reported invalid.
func TestDoctorMetricsDown(t *testing.T) {
	doctorEnv(t)
	var out bytes.Buffer
	if runDoctor(context.Background(), &out, DoctorOptions{}, doctorTestDeps(&doctorMetrics{down: true}, &doctorChannel{}, ""), discard()) {
		t.Error("passed with the metrics backend down")
	}
	for _, want := range []string{
		"✗ metrics backend         prometheus does not answer: dial tcp: connection refused",
		"- shop/api                templates not checked: no metrics backend",
	} {
		if !has(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
