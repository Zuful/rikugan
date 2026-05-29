package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

type fakeMetrics struct {
	samples []provider.MetricSample
	baseRep string
	names   []string
	err     error
}

func (f *fakeMetrics) Name() string                     { return "fake" }
func (f *fakeMetrics) Tools() []provider.ToolDefinition { return nil }

func (f *fakeMetrics) GetMetric(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]provider.MetricSample, error) {
	return f.samples, f.err
}

func (f *fakeMetrics) CompareToBaseline(_ context.Context, _ string, _, _ time.Duration) (string, error) {
	return f.baseRep, f.err
}

func (f *fakeMetrics) ListAvailableMetrics(_ context.Context, _ string) ([]string, error) {
	return f.names, f.err
}

func TestConceptProvider_ExposesThreeCanonicalTools(t *testing.T) {
	cp := NewConceptProvider(&fakeMetrics{}, "fake", nil)
	names := map[string]bool{}
	for _, td := range cp.Tools() {
		names[td.Tool.Name] = true
	}
	for _, want := range []string{"get_metric", "compare_to_baseline", "list_available_metrics"} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
	if cp.Name() != "metrics" {
		t.Errorf("Name: got %q, want metrics", cp.Name())
	}
}

func TestBuildGetMetricReport_GroupsByLabelFingerprint(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	samples := []provider.MetricSample{
		{Time: now, Labels: map[string]string{"job": "api"}, Value: 1},
		{Time: now.Add(time.Second), Labels: map[string]string{"job": "api"}, Value: 2},
		{Time: now, Labels: map[string]string{"job": "web"}, Value: 9},
	}
	report := buildGetMetricReport("prometheus", "up", now, now.Add(time.Minute), time.Second, samples)
	for _, want := range []string{
		"## Metric Query Results",
		"**Backend:** `prometheus`",
		"**Series returned:** 2",
		`{job="api"}`,
		`{job="web"}`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in report:\n%s", want, report)
		}
	}
}

func TestBuildGetMetricReport_EmptyShowsNoSamplesMarker(t *testing.T) {
	report := buildGetMetricReport("prometheus", "up", time.Now(), time.Now(), time.Second, nil)
	if !strings.Contains(report, "_No samples returned._") {
		t.Errorf("missing empty marker:\n%s", report)
	}
}

func TestBuildListMetricsReport_EmitsBackendAndPrefix(t *testing.T) {
	report := buildListMetricsReport("prometheus", "http_", []string{"http_a", "http_b"})
	for _, want := range []string{
		"## Available Metrics",
		"**Backend:** `prometheus`",
		"**Prefix:** `http_`",
		"**Returned:** 2",
		"- `http_a`",
		"- `http_b`",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in report:\n%s", want, report)
		}
	}
}

func TestFingerprintLabels_StableAcrossMaps(t *testing.T) {
	a := fingerprintLabels(map[string]string{"x": "1", "y": "2"})
	b := fingerprintLabels(map[string]string{"y": "2", "x": "1"})
	if a != b {
		t.Errorf("fingerprint not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "{") || !strings.HasSuffix(a, "}") {
		t.Errorf("missing braces: %q", a)
	}
}
