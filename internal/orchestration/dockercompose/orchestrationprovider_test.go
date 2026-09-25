package dockercompose

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// stubRunner is a CommandRunner that returns canned responses keyed
// by the first arg passed (since every docker invocation starts with
// a subcommand). It records each call so tests can assert what was run.
type stubRunner struct {
	responses map[string]string // key = subcommand (e.g. "compose", "inspect", "events")
	calls     [][]string
	err       error
}

func (s *stubRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, append([]string{name}, args...))
	if s.err != nil {
		return nil, s.err
	}
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	resp, ok := s.responses[key]
	if !ok {
		return nil, nil
	}
	return []byte(resp), nil
}

// composePSNDJSON is a realistic Compose 2.x `ps --format json` output
// with two services: api (2 replicas, one running healthy, one
// unhealthy) and worker (1 replica, exited).
const composePSNDJSON = `{"ID":"c1","Name":"web_api_1","Project":"web","Service":"api","State":"running","Health":"healthy","Image":"api:1.0","Created":1700000000}
{"ID":"c2","Name":"web_api_2","Project":"web","Service":"api","State":"running","Health":"unhealthy","Image":"api:1.0","Created":1700000010}
{"ID":"c3","Name":"web_worker_1","Project":"web","Service":"worker","State":"exited","ExitCode":1,"Image":"worker:2.3","Created":1699999000}`

func TestProvider_ListServices_CollapsesReplicasIntoOneService(t *testing.T) {
	t.Parallel()
	r := &stubRunner{responses: map[string]string{"compose": composePSNDJSON}}
	p := New(WithRunner(r))
	svcs, err := p.ListServices(context.Background(), "web")
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("expected 2 services (api+worker), got %d", len(svcs))
	}
	names := map[string]bool{svcs[0].Name: true, svcs[1].Name: true}
	if !names["api"] || !names["worker"] {
		t.Errorf("missing service names, got %v", names)
	}
	for _, s := range svcs {
		if s.Kind != kindDockerComposeService {
			t.Errorf("service %s kind=%q, want %q", s.Name, s.Kind, kindDockerComposeService)
		}
		if s.Namespace != "web" {
			t.Errorf("service %s namespace=%q, want web", s.Name, s.Namespace)
		}
	}
}

func TestProvider_GetServiceState_DegradedOnUnhealthyReplica(t *testing.T) {
	t.Parallel()
	r := &stubRunner{responses: map[string]string{"compose": composePSNDJSON}}
	p := New(WithRunner(r))
	state, err := p.GetServiceState(context.Background(), provider.Service{Name: "api", Namespace: "web"})
	if err != nil {
		t.Fatalf("GetServiceState: %v", err)
	}
	if state.Status != "degraded" {
		t.Errorf("expected degraded (one replica unhealthy), got %q", state.Status)
	}
	if state.Replicas.Available != 2 || state.Replicas.Desired != 2 {
		t.Errorf("replicas %d/%d, want 2/2", state.Replicas.Available, state.Replicas.Desired)
	}
	if !strings.Contains(state.Message, "unhealthy") {
		t.Errorf("expected unhealthy mention in message: %q", state.Message)
	}
}

func TestProvider_GetServiceState_FailedOnExitedReplica(t *testing.T) {
	t.Parallel()
	r := &stubRunner{responses: map[string]string{"compose": composePSNDJSON}}
	p := New(WithRunner(r))
	state, err := p.GetServiceState(context.Background(), provider.Service{Name: "worker", Namespace: "web"})
	if err != nil {
		t.Fatalf("GetServiceState: %v", err)
	}
	if state.Status != "failed" {
		t.Errorf("expected failed, got %q", state.Status)
	}
}

func TestProvider_GetServiceState_UnknownWhenNoContainers(t *testing.T) {
	t.Parallel()
	r := &stubRunner{responses: map[string]string{"compose": ""}}
	p := New(WithRunner(r))
	state, err := p.GetServiceState(context.Background(), provider.Service{Name: "missing", Namespace: "web"})
	if err != nil {
		t.Fatalf("GetServiceState: %v", err)
	}
	if state.Status != "unknown" {
		t.Errorf("expected unknown when service absent, got %q", state.Status)
	}
}

func TestProvider_RestartService_InvokesDockerComposeRestart(t *testing.T) {
	t.Parallel()
	r := &stubRunner{responses: map[string]string{}}
	p := New(WithRunner(r))
	if err := p.RestartService(context.Background(), provider.Service{Name: "api", Namespace: "web"}); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 docker invocation, got %d (%v)", len(r.calls), r.calls)
	}
	call := r.calls[0]
	want := []string{"docker", "compose", "-p", "web", "restart", "api"}
	for i, w := range want {
		if call[i] != w {
			t.Errorf("call[%d]=%q, want %q (full: %v)", i, call[i], w, call)
		}
	}
}

func TestProvider_GetServiceHistory_GracefullyDegradesWithExplanation(t *testing.T) {
	t.Parallel()
	r := &stubRunner{responses: map[string]string{"compose": composePSNDJSON}}
	p := New(WithRunner(r))
	report, err := p.GetServiceHistory(context.Background(), provider.Service{Name: "api", Namespace: "web"})
	if err != nil {
		t.Fatalf("GetServiceHistory: %v", err)
	}
	for _, want := range []string{
		"## Service History — `api` (Docker Compose)",
		"Docker Compose does not record rolling-update history natively",
		"`api:1.0`",
		"**Alternatives to recover the equivalent information manually:**",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in report:\n%s", want, report)
		}
	}
}

func TestProvider_GetRecentEvents_ParsesDockerNDJSON(t *testing.T) {
	t.Parallel()
	composeOut := `{"ID":"c1","Name":"web_api_1","Project":"web","Service":"api","State":"running","Image":"api:1.0"}`
	eventsOut := `{"Type":"container","Action":"die","time":1700000200,"Actor":{"ID":"c1","Attributes":{"name":"web_api_1"}}}
{"Type":"container","Action":"start","time":1700000210,"Actor":{"ID":"c1","Attributes":{"name":"web_api_1"}}}`
	r := &stubRunner{
		responses: map[string]string{
			"compose": composeOut,
			"events":  eventsOut,
		},
	}
	p := New(WithRunner(r))
	events, err := p.GetRecentEvents(context.Background(), provider.Service{Name: "api", Namespace: "web"}, time.Hour)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	// Newest first.
	if events[0].Reason != "start" {
		t.Errorf("expected newest event 'start', got %q", events[0].Reason)
	}
}

func TestProvider_SatisfiesOrchestrationProviderInterface(t *testing.T) {
	t.Parallel()
	// Compile-time check is in dockercompose.go; this guard exists so
	// `go test` flags any future drift from the interface contract.
	var _ provider.OrchestrationProvider = (*Provider)(nil)
}

func TestParseComposePS_HandlesArrayForm(t *testing.T) {
	t.Parallel()
	arr := `[{"ID":"x","Project":"p","Service":"s","State":"running","Image":"img"}]`
	got := parseComposePS([]byte(arr))
	if len(got) != 1 || got[0].Service != "s" {
		t.Errorf("array form not parsed correctly: %+v", got)
	}
}

func TestValidateComposeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"plain name", "checkout", false},
		{"digits and dashes", "api-v2", false},
		{"dots and underscores", "my_project.v1", false},
		{"empty means not supplied", "", false},
		{"leading dash would be read as a flag", "-v", true},
		{"long flag shape", "--volume=/:/host", true},
		{"embedded space", "api service", true},
		{"shell metacharacter", "api;rm -rf /", true},
		{"command substitution", "$(whoami)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateComposeName("service", tt.input)
			if tt.wantErr && err == nil {
				t.Errorf("validateComposeName(%q) = nil, want an error", tt.input)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateComposeName(%q) = %v, want nil", tt.input, err)
			}
		})
	}
}

func TestRestartService_RejectsFlagLikeServiceName(t *testing.T) {
	t.Parallel()
	runner := &stubRunner{}
	p := New(WithRunner(runner), WithProject("demo"))

	err := p.RestartService(context.Background(), provider.Service{Name: "-v", Namespace: "demo"})
	if err == nil {
		t.Fatal("expected a validation error for a flag-like service name")
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner was invoked %d time(s); validation must happen before exec", len(runner.calls))
	}
}
