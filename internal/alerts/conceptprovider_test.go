package alerts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

type fakeAlerts struct {
	alerts []provider.Alert
	detail provider.Alert
	err    error
	called string
	lastID string
	lastSv string
}

func (f *fakeAlerts) Name() string                     { return "fake" }
func (f *fakeAlerts) Tools() []provider.ToolDefinition { return nil }

func (f *fakeAlerts) ListActiveAlerts(_ context.Context, svc string) ([]provider.Alert, error) {
	f.called = "list"
	f.lastSv = svc
	return f.alerts, f.err
}

func (f *fakeAlerts) GetAlertDetails(_ context.Context, id string) (provider.Alert, error) {
	f.called = "detail"
	f.lastID = id
	return f.detail, f.err
}

func TestConceptProvider_RegistersTwoCanonicalTools(t *testing.T) {
	t.Parallel()
	cp := NewConceptProvider(&fakeAlerts{}, "fake", nil)
	names := map[string]bool{}
	for _, td := range cp.Tools() {
		names[td.Tool.Name] = true
	}
	for _, want := range []string{"list_active_alerts", "get_alert_details"} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
	if cp.Name() != "alerts" {
		t.Errorf("Name: got %q, want alerts", cp.Name())
	}
}

func TestBuildListAlertsReport_SortsBySeverityThenStartedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	alerts := []provider.Alert{
		{ID: "1", Title: "info-event", Severity: "info", State: "firing", StartedAt: now},
		{ID: "2", Title: "old-critical", Severity: "critical", State: "firing", StartedAt: now.Add(-time.Hour)},
		{ID: "3", Title: "fresh-critical", Severity: "critical", State: "firing", StartedAt: now},
	}
	report := buildListAlertsReport("alertmanager", "checkout", alerts)
	// Expect critical first, with the newer critical above the older one.
	freshIdx := strings.Index(report, "fresh-critical")
	oldIdx := strings.Index(report, "old-critical")
	infoIdx := strings.Index(report, "info-event")
	if freshIdx == -1 || oldIdx == -1 || infoIdx == -1 {
		t.Fatalf("missing titles in report:\n%s", report)
	}
	if !(freshIdx < oldIdx && oldIdx < infoIdx) {
		t.Errorf("ordering wrong: fresh=%d old=%d info=%d\n%s", freshIdx, oldIdx, infoIdx, report)
	}
	if !strings.Contains(report, "**Backend:** `alertmanager`") {
		t.Errorf("backend not in report")
	}
	if !strings.Contains(report, "**Service:** `checkout`") {
		t.Errorf("service not in report")
	}
}

func TestBuildListAlertsReport_EmptyShowsNoAlertsMarker(t *testing.T) {
	t.Parallel()
	report := buildListAlertsReport("pagerduty", "", nil)
	if !strings.Contains(report, "_No active alerts._") {
		t.Errorf("missing empty marker:\n%s", report)
	}
}

func TestBuildAlertDetailReport_SerializesExtrasAsJSON(t *testing.T) {
	t.Parallel()
	a := provider.Alert{
		ID:        "fp-123",
		Title:     "HighErrorRate",
		Severity:  "critical",
		State:     "firing",
		StartedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Service:   "checkout",
		Source:    "alertmanager",
		Extras: map[string]any{
			"runbook": "https://runbook/checkout",
			"team":    "payments",
		},
	}
	report := buildAlertDetailReport("alertmanager", a)
	for _, want := range []string{
		"## Alert `fp-123`",
		"**Title:** HighErrorRate",
		"| Severity | critical |",
		"### Backend-specific extras",
		`"runbook"`,
		`"team"`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in report:\n%s", want, report)
		}
	}
}

func TestSeverityRank_OrderingIsStable(t *testing.T) {
	t.Parallel()
	if severityRank("critical") <= severityRank("warning") {
		t.Errorf("critical must outrank warning")
	}
	if severityRank("warning") <= severityRank("info") {
		t.Errorf("warning must outrank info")
	}
	if severityRank("info") <= severityRank("") {
		t.Errorf("info must outrank empty/unknown")
	}
	if severityRank("CRITICAL") != severityRank("critical") {
		t.Errorf("rank should be case-insensitive")
	}
}
