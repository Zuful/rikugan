package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

type metricCall struct {
	query      string
	start, end time.Time
}

// fakeMetrics answers GetMetric from per-signal value functions, picked
// by the first substring the query contains — in order, because the
// error-rate query also contains the request-rate metric name. Values
// are a function of the sample's time, so a seasonal query sees last
// week's values.
type fakeMetrics struct {
	provider.MetricsProvider
	values []metricValue
	labels map[string]int // number of series to return per query substring (default 1)
	calls  []metricCall
	// notBefore, when set, is the store's retention: nothing older.
	notBefore time.Time
	// fail, when set, fails every query containing failMatch (every
	// query when failMatch is empty).
	fail      error
	failMatch string
}

func (f *fakeMetrics) Name() string { return "prometheus" }

type metricValue struct {
	match string
	fn    func(t time.Time) float64
}

func (f *fakeMetrics) GetMetric(_ context.Context, query string, start, end time.Time, step time.Duration) ([]provider.MetricSample, error) {
	f.calls = append(f.calls, metricCall{query, start, end})
	if f.fail != nil && strings.Contains(query, f.failMatch) {
		return nil, f.fail
	}
	if !f.notBefore.IsZero() && end.Before(f.notBefore) {
		return nil, nil
	}
	for _, v := range f.values {
		key, fn := v.match, v.fn
		if !strings.Contains(query, key) {
			continue
		}
		n := 1
		if f.labels[key] > 0 {
			n = f.labels[key]
		}
		var out []provider.MetricSample
		for t := start; !t.After(end); t = t.Add(step) {
			for i := 0; i < n; i++ {
				out = append(out, provider.MetricSample{Time: t, Value: fn(t), Labels: map[string]string{"i": string(rune('a' + i))}})
			}
		}
		return out, nil
	}
	return nil, nil
}

type fakeOrch struct {
	provider.OrchestrationProvider
	instances []provider.Instance
	revision  string
	err       error
}

func (f *fakeOrch) Name() string { return "kubernetes" }

func (f *fakeOrch) GetRevisionInstances(_ context.Context, _ provider.Service, revision string) ([]provider.Instance, error) {
	f.revision = revision
	return f.instances, f.err
}

type fakeDeployments struct {
	provider.DeploymentSource
	rollout provider.Rollout
}

func (f *fakeDeployments) LastRollout(context.Context, provider.Service) (provider.Rollout, error) {
	return f.rollout, nil
}

var svcAPI = provider.Service{Name: "api", Namespace: "prod"}

func newCollector(m *fakeMetrics, r provider.Rollout, now time.Time) (*Collector, *fakeOrch) {
	orch := &fakeOrch{instances: readyInstances(3)}
	return &Collector{
		Metrics:       m,
		Orchestration: orch,
		Deployments:   &fakeDeployments{rollout: r},
		Now:           func() time.Time { return now },
	}, orch
}

// busy reproduces healthy() through the default templates' metric names.
func busy(latencyAfter float64) *fakeMetrics {
	return &fakeMetrics{values: []metricValue{
		{`code=~"5.."`, func(time.Time) float64 { return perMinute(1200) * 0.002 }},
		{"http_requests_total", func(time.Time) float64 { return perMinute(1200) }},
		{"http_request_duration_seconds", func(t time.Time) float64 {
			if t.Before(completed) {
				return 0.120
			}
			return latencyAfter
		}},
		{"container_cpu_usage_seconds", func(time.Time) float64 { return 0.25 }},
		{"container_memory_working_set", func(time.Time) float64 { return 256 << 20 }},
	}}
}

func TestCollector_CompletedRollout(t *testing.T) {
	m := busy(0.450)
	c, orch := newCollector(m, rollout(), minute(15).Add(10*time.Second))

	r, err := c.Check(context.Background(), svcAPI)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if r.Outcome != OutcomeEvaluated || r.Checkpoint != 15*time.Minute || r.Verdict != StatusDegraded {
		t.Fatalf("got outcome %s checkpoint %s verdict %s\n%s", r.Outcome, r.Checkpoint, r.Verdict, dump(r))
	}
	if lat, _ := r.Signal(SignalLatency); lat.Status != StatusDegraded {
		t.Errorf("latency: %+v", lat)
	}
	if e, _ := r.Signal(SignalErrorRate); e.Status != StatusOK || !strings.Contains(e.Reason, "0.20%") {
		t.Errorf("error rate must come from the 5xx query: %+v", e)
	}
	if orch.revision != "abc123" {
		t.Errorf("instances read for revision %q, want the rollout's", orch.revision)
	}

	// Five current queries over [start-30m, completion+15m], the
	// seasonal request rate over the same range a week earlier, then the
	// p95 of each window's requests, at its end.
	if len(m.calls) != 8 {
		t.Fatalf("got %d metric queries, want 8: %+v", len(m.calls), m.calls)
	}
	for i, w := range []struct {
		at     time.Time
		window string
	}{{minute(-2), "[1800s]"}, {minute(15), "[900s]"}} {
		call := m.calls[6+i]
		if !call.start.Equal(w.at) || !call.end.Equal(w.at) || !strings.Contains(call.query, "increase(http_request_duration_seconds_bucket") || !strings.Contains(call.query, w.window) {
			t.Errorf("window p95 query %+v, want %s at %s", call, w.window, w.at)
		}
	}
	if lat, _ := r.Signal(SignalLatency); !strings.Contains(lat.Reason, "p95 over the window 120ms → 450ms") || strings.Contains(lat.Reason, "median") {
		t.Errorf("latency must come from the window p95: %+v", lat)
	}
	wantFrom, wantTo := minute(-32), minute(15)
	for _, call := range m.calls[:5] {
		if !call.start.Equal(wantFrom) || !call.end.Equal(wantTo) {
			t.Errorf("query %q over [%s, %s], want [%s, %s]", call.query, call.start, call.end, wantFrom, wantTo)
		}
		if !strings.Contains(call.query, `namespace="prod"`) {
			t.Errorf("query not scoped to the namespace: %s", call.query)
		}
	}
	season := m.calls[5]
	if !season.start.Equal(wantFrom.Add(-seasonalOffset)) || !strings.Contains(season.query, "http_requests_total") {
		t.Errorf("seasonal query %+v", season)
	}
	if _, ok := r.Signal(SignalTraffic); !ok {
		t.Error("traffic not evaluated")
	}
	if strings.Contains(strings.Join(r.Notes, " "), "seasonality guard disabled") {
		t.Errorf("seasonal data was available: notes %q", r.Notes)
	}
}

func TestCollector_NoMetricsWithoutAVerdict(t *testing.T) {
	m := busy(0.120)
	r := rollout()
	r.State, r.CompletedAt = provider.RolloutInProgress, time.Time{}
	c, _ := newCollector(m, r, minute(3))

	rep, err := c.Check(context.Background(), svcAPI)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Outcome != OutcomeInProgress || len(m.calls) != 0 {
		t.Fatalf("outcome %s after %d metric queries, want in_progress and none", rep.Outcome, len(m.calls))
	}
}

func TestCollector_UnaggregatedQueryIsSkipped(t *testing.T) {
	m := busy(0.120)
	m.labels = map[string]int{"http_request_duration_seconds": 3}
	c, _ := newCollector(m, rollout(), minute(15).Add(10*time.Second))

	r, err := c.Check(context.Background(), svcAPI)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if lat, _ := r.Signal(SignalLatency); lat.Status != StatusNotEvaluated {
		t.Errorf("latency from a 3-series query: %+v", lat)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "latency window p95 query returned 3 series") {
		t.Errorf("notes %q", r.Notes)
	}
}

func TestRender_RefusesUnsafeNames(t *testing.T) {
	tpl := DefaultTemplates().RequestRate
	if _, err := render(tpl, templateVars{Namespace: "prod", Service: "api"}); err != nil {
		t.Fatalf("plain names refused: %v", err)
	}
	for _, bad := range []templateVars{
		{Namespace: "prod", Service: `api"} or vector(1) #`},
		{Namespace: `prod",x="`, Service: "api"},
		{Namespace: "prod", Service: "api", Revision: "abc|.*"},
	} {
		if q, err := render(tpl, bad); err == nil {
			t.Errorf("%+v rendered to %s", bad, q)
		}
	}
}

func TestSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.yaml")
	if err := os.WriteFile(path, []byte(`
defaults:
  thresholds:
    min_requests: 100
  templates:
    request_rate: sum(rate(http_server_requests_seconds_count{app="{{.Service}}"}[1m]))
services:
  prod/checkout:
    thresholds:
      min_requests: 50
      rollout_unavailable_grace: 90s
      blunt_rule: false
      latency_degraded_from_checkpoint: 5m
      latency_method: median
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}

	cfg, tpl := s.For(provider.Service{Namespace: "prod", Name: "api"})
	if cfg.MinRequests != 100 || cfg.RolloutUnavailableGrace != 60*time.Second || !cfg.BluntRule {
		t.Errorf("defaults section not applied alone: %+v", cfg)
	}
	if !strings.Contains(tpl.RequestRate, "http_server_requests_seconds_count") || tpl.ErrorRate != DefaultTemplates().ErrorRate {
		t.Errorf("templates: %+v", tpl)
	}

	cfg, _ = s.For(provider.Service{Namespace: "prod", Name: "checkout"})
	if cfg.MinRequests != 50 || cfg.RolloutUnavailableGrace != 90*time.Second || cfg.UnavailableGrace != 2*time.Minute ||
		cfg.BluntRule || cfg.LatencyDegradedFromCheckpoint != 5*time.Minute || cfg.LatencyMethod != LatencyMedian {
		t.Errorf("service section: %+v", cfg)
	}
	if cfg, _ := s.For(provider.Service{Namespace: "prod", Name: "api"}); cfg.LatencyMethod != LatencyWindow {
		t.Errorf("default latency method %q", cfg.LatencyMethod)
	}

	if err := os.WriteFile(path, []byte("services:\n  prod/api:\n    thresholds:\n      latency_method: mean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil || !strings.Contains(err.Error(), `latency_method "mean"`) {
		t.Errorf("an unknown latency method must be rejected: %v", err)
	}

	if err := os.WriteFile(path, []byte("defaults:\n  thresholds:\n    min_request: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Error("a misspelled key must be rejected, not silently ignored")
	}
}

func TestCollector_CheckRolloutWithOptions(t *testing.T) {
	// Daemon catch-up at +40 for the +15 checkpoint, after a rollout that
	// started 10 minutes after the previous one.
	m := busy(0.120)
	c, _ := newCollector(m, rollout(), minute(40))
	beforeEnd := rollout().StartedAt.Add(-10 * time.Minute)

	r, err := c.CheckRollout(context.Background(), rollout(), CheckOptions{Checkpoint: 15 * time.Minute, BeforeEnd: beforeEnd})
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint != 15*time.Minute || r.Verdict != StatusOK {
		t.Fatalf("checkpoint %s verdict %s\n%s", r.Checkpoint, r.Verdict, dump(r))
	}
	wantFrom, wantTo := beforeEnd.Add(-30*time.Minute), minute(15)
	if call := m.calls[0]; !call.start.Equal(wantFrom) || !call.end.Equal(wantTo) {
		t.Errorf("query over [%s, %s], want [%s, %s]", call.start, call.end, wantFrom, wantTo)
	}
}

func TestCollector_MetricsBackendFailing(t *testing.T) {
	down := errors.New("dial tcp 10.0.0.9:9090: connection refused")
	m := busy(0.450)
	m.fail = down
	c, orch := newCollector(m, rollout(), minute(15).Add(10*time.Second))

	// Healthy instances: nothing sound to say.
	r, err := c.CheckRollout(context.Background(), rollout(), CheckOptions{Checkpoint: 15 * time.Minute})
	var be *BackendError
	if !errors.As(err, &be) || be.Kind != BackendMetrics || be.Name != "prometheus" || !errors.Is(err, down) {
		t.Fatalf("error %v, want a metrics BackendError", err)
	}
	if r.Verdict != StatusInsufficientData {
		t.Errorf("verdict %s, want insufficient data\n%s", r.Verdict, dump(r))
	}
	for _, name := range []string{SignalLatency, SignalErrorRate, SignalSaturation, SignalTraffic} {
		if s, _ := r.Signal(name); s.Status != StatusNotEvaluated || !strings.Contains(s.Reason, "metrics backend prometheus failing") {
			t.Errorf("%s: %+v", name, s)
		}
	}
	notes := strings.Join(r.Notes, "\n")
	if !strings.Contains(notes, "metrics not evaluated") || strings.Contains(notes, "query failed") {
		t.Errorf("notes %q: want one note for the backend, not one per query", r.Notes)
	}

	// A crash loop is degraded on the instance signals alone.
	orch.instances[0].CrashLooping = true
	r, err = c.CheckRollout(context.Background(), rollout(), CheckOptions{Checkpoint: 15 * time.Minute})
	if !errors.As(err, &be) || r.Verdict != StatusDegraded {
		t.Errorf("crash loop with metrics down: verdict %s, err %v", r.Verdict, err)
	}

	// The MCP tool shows the report rather than an error.
	if r, err := c.Check(context.Background(), svcAPI); err != nil || r.Outcome != OutcomeEvaluated {
		t.Errorf("Check: %v, outcome %s", err, r.Outcome)
	}

	// The sweep too.
	c.Now = func() time.Time { return sweepNow }
	r, _, err = c.CheckSweep(context.Background(), svcAPI, &PreviousSweep{At: sweepNow.Add(-4 * time.Hour)})
	if !errors.As(err, &be) || be.Kind != BackendMetrics {
		t.Fatalf("sweep: error %v, want a metrics BackendError", err)
	}
	if s, _ := r.Signal(SignalMemoryLeak); s.Status != StatusNotEvaluated {
		t.Errorf("sweep leak signal: %+v", s)
	}
	if r.Verdict != StatusDegraded {
		t.Errorf("sweep verdict %s, want degraded on the crash loop", r.Verdict)
	}
}

func TestCollector_OneQueryFailingIsANote(t *testing.T) {
	m := busy(0.120)
	m.fail, m.failMatch = errors.New("bad_data: parse error"), "container_cpu_usage_seconds"
	c, _ := newCollector(m, rollout(), minute(15).Add(10*time.Second))
	r, err := c.CheckRollout(context.Background(), rollout(), CheckOptions{Checkpoint: 15 * time.Minute})
	if err != nil {
		t.Fatalf("one failing query is no backend outage: %v", err)
	}
	if r.Verdict != StatusOK || !strings.Contains(strings.Join(r.Notes, "\n"), "CPU query failed") {
		t.Errorf("verdict %s, notes %q", r.Verdict, r.Notes)
	}
}

func TestCollector_OrchestrationFailing(t *testing.T) {
	c, orch := newCollector(busy(0.120), rollout(), minute(15).Add(10*time.Second))
	orch.err = errors.New("the server has asked for the client to provide credentials")
	var be *BackendError
	if _, err := c.CheckRollout(context.Background(), rollout(), CheckOptions{}); !errors.As(err, &be) || be.Kind != BackendOrchestration || be.Name != "kubernetes" {
		t.Errorf("rollout: %v", err)
	}
	if _, _, err := c.CheckSweep(context.Background(), svcAPI, nil); !errors.As(err, &be) || be.Kind != BackendOrchestration {
		t.Errorf("sweep: %v", err)
	}
}
