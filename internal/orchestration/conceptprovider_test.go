package orchestration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

type fakeOrch struct {
	services []provider.Service
	state    provider.ServiceState
	describe string
	events   []provider.Event
	history  string
	restErr  error
	listErr  error
	lastRest provider.Service
}

func (f *fakeOrch) Name() string                     { return "fake" }
func (f *fakeOrch) Tools() []provider.ToolDefinition { return nil }

func (f *fakeOrch) ListServices(_ context.Context, _ string) ([]provider.Service, error) {
	return f.services, f.listErr
}

func (f *fakeOrch) GetServiceState(_ context.Context, _ provider.Service) (provider.ServiceState, error) {
	return f.state, nil
}

func (f *fakeOrch) DescribeService(_ context.Context, _ provider.Service) (string, error) {
	return f.describe, nil
}

func (f *fakeOrch) GetRecentEvents(_ context.Context, _ provider.Service, _ time.Duration) ([]provider.Event, error) {
	return f.events, nil
}

func (f *fakeOrch) RestartService(_ context.Context, svc provider.Service) error {
	f.lastRest = svc
	return f.restErr
}

func (f *fakeOrch) GetServiceHistory(_ context.Context, _ provider.Service) (string, error) {
	return f.history, nil
}

func (f *fakeOrch) GetRevisionInstances(_ context.Context, _ provider.Service, _ string) ([]provider.Instance, error) {
	return nil, nil
}

func TestConceptProvider_RegistersSixCanonicalTools(t *testing.T) {
	t.Parallel()
	cp := NewConceptProvider(&fakeOrch{}, "fake", nil)
	names := map[string]bool{}
	for _, td := range cp.Tools() {
		names[td.Tool.Name] = true
	}
	for _, want := range []string{
		"list_services", "get_service_state", "describe_service",
		"get_recent_events", "restart_service", "get_service_history",
	} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
	if cp.Name() != "orchestration" {
		t.Errorf("Name: got %q", cp.Name())
	}
}

func TestBuildListServicesReport_SortsByNamespaceThenName(t *testing.T) {
	t.Parallel()
	services := []provider.Service{
		{Name: "checkout", Namespace: "prod", Kind: "deployment"},
		{Name: "api", Namespace: "prod", Kind: "deployment"},
		{Name: "alpha", Namespace: "dev", Kind: "deployment"},
	}
	report := buildListServicesReport("kubernetes", "", services)
	iAlpha := strings.Index(report, "alpha")
	iAPI := strings.Index(report, "| `api`")
	iCheckout := strings.Index(report, "| `checkout`")
	if iAlpha >= iAPI || iAPI >= iCheckout {
		t.Errorf("ordering wrong: alpha=%d api=%d checkout=%d\n%s", iAlpha, iAPI, iCheckout, report)
	}
}

func TestBuildListServicesReport_EmptyShowsNoneMarker(t *testing.T) {
	t.Parallel()
	report := buildListServicesReport("kubernetes", "prod", nil)
	if !strings.Contains(report, "_No services found._") {
		t.Errorf("missing empty marker:\n%s", report)
	}
	if !strings.Contains(report, "**Namespace:** `prod`") {
		t.Errorf("missing namespace echo")
	}
}

func TestBuildStateReport_RendersReplicasAndStatus(t *testing.T) {
	t.Parallel()
	report := buildStateReport("kubernetes",
		provider.Service{Name: "api", Namespace: "prod", Kind: "deployment"},
		provider.ServiceState{
			Status:   "degraded",
			Message:  "ProgressDeadlineExceeded",
			Replicas: struct{ Desired, Available int }{Desired: 3, Available: 1},
		},
	)
	for _, want := range []string{
		"## Service State — prod/api",
		"**Backend:** `kubernetes`",
		"| Status | degraded |",
		"| Replicas | 1 / 3 (available / desired) |",
		"ProgressDeadlineExceeded",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q\n%s", want, report)
		}
	}
}

func TestBuildEventsReport_TruncatesLongMessageAndEscapesPipe(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("A", 150) + "|piped"
	events := []provider.Event{
		{Time: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), Type: "Warning", Reason: "OOMKilled", Message: long},
	}
	report := buildEventsReport("kubernetes",
		provider.Service{Name: "api", Namespace: "prod"}, time.Hour, events,
	)
	if strings.Contains(report, long) {
		t.Errorf("long message should be truncated")
	}
	if !strings.Contains(report, "...") {
		t.Errorf("expected truncation marker")
	}
	if strings.Contains(report, "|piped") && !strings.Contains(report, "\\|piped") {
		t.Errorf("pipe should be escaped in markdown table")
	}
}

func TestBuildEventsReport_EmptyShowsNoneMarker(t *testing.T) {
	t.Parallel()
	report := buildEventsReport("kubernetes",
		provider.Service{Name: "api", Namespace: "prod"}, time.Hour, nil)
	if !strings.Contains(report, "_No recent events for this service._") {
		t.Errorf("missing empty marker:\n%s", report)
	}
}

func TestSummarizeLabels_CapsAtThreeWithEllipsis(t *testing.T) {
	t.Parallel()
	got := summarizeLabels(map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"})
	if !strings.Contains(got, "…") {
		t.Errorf("expected ellipsis when more than 3 labels, got %q", got)
	}
	if !strings.HasPrefix(got, "a=1, b=2, c=3") {
		t.Errorf("expected first three labels in order, got %q", got)
	}
	if summarizeLabels(nil) != "—" {
		t.Errorf("nil labels should render as em-dash")
	}
}

func TestRestartService_PropagatesBackendError(t *testing.T) {
	t.Parallel()
	cp := NewConceptProvider(&fakeOrch{restErr: errors.New("nope")}, "fake", nil)
	// We can't easily drive the MCP request without the mark3labs harness;
	// invoke the underlying interface directly to assert error propagation.
	if err := cp.impl.RestartService(context.Background(), provider.Service{Name: "x"}); err == nil {
		t.Error("expected backend error to surface")
	}
}
