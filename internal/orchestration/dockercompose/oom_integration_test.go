//go:build integration

// This test runs against a real Docker daemon. It answers one question
// the stubbed runner cannot: once a container that was OOM-killed has
// been restarted by its restart policy, does docker inspect still say
// so? If State.OOMKilled (and ExitCode) are reset when the container
// starts again, the last-termination record GetRevisionInstances builds
// from them loses the OOM.
//
// Run locally:
//
//	go test -tags integration -run Integration -v ./internal/orchestration/dockercompose/...
package dockercompose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// The service OOMs on its first run only: it drops a marker in its
// writable layer (which survives a restart of the same container), then
// allocates far past its memory limit. Restarted by on-failure, it finds
// the marker and stays up, so the test can read the state of a running
// container whose previous run was an OOM kill.
const oomCompose = `services:
  leak:
    image: busybox:1.36
    mem_limit: 16m
    memswap_limit: 16m
    restart: on-failure
    command: ["sh", "-c", "if [ -f /oomed ]; then exec sleep 3600; fi; touch /oomed; dd if=/dev/zero of=/dev/null bs=128M count=1"]
`

// Repeated because Docker is not consistent: in about one run in ten
// it emits no oom event for a genuine OOM kill (cgroup v2 runners), so
// a single run says little either way. Each run checks what the
// provider can guarantee: the OOM is attested whenever Docker reported
// it, and otherwise the exit reads as a SIGKILL, never as the clean
// exit docker inspect claims after the restart.
const oomRuns = 10

func TestIntegration_OOMKilledAfterRestart(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	for i := 0; i < oomRuns; i++ {
		t.Run(fmt.Sprintf("run %d", i+1), func(t *testing.T) { oomScenario(t, fmt.Sprintf("rikugan-oom-probe-%d", i+1)) })
	}
}

func oomScenario(t *testing.T, project string) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(oomCompose), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", project, "-f", filepath.Join(dir, "compose.yaml")}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker compose %v: %v\n%s", args, err, out)
		}
	}
	compose("up", "-d")
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", project, "-f", filepath.Join(dir, "compose.yaml"), "down", "-v", "-t", "0").Run()
	})

	p := New(WithProject(project))
	svc := provider.Service{Name: "leak", Namespace: project}

	var inst provider.Instance
	deadline := time.Now().Add(2 * time.Minute)
	for {
		got, err := p.GetRevisionInstances(ctx, svc, "")
		if err == nil && len(got) == 1 && got[0].RestartCount >= 1 && got[0].Ready {
			inst = got[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("container never came back up after its first run (last: %+v, err %v)", got, err)
		}
		time.Sleep(time.Second)
	}

	raw := rawState(ctx, t, project)
	t.Logf("docker inspect State after restart: %s", raw)
	t.Logf("instance: %+v", inst)

	if len(inst.Terminations) != 1 {
		t.Fatalf("want one termination record for the OOM-killed first run, got %+v", inst.Terminations)
	}
	events := rawEvents(ctx, t, project)
	t.Logf("docker events: %s", events)
	term := inst.Terminations[0]
	switch {
	case strings.Contains(events, "oom@"):
		if !term.OOM || term.Reason != "OOMKilled" {
			t.Errorf("Docker reported the OOM, but the instance reports %+v", term)
		}
	default:
		// Docker dropped the oom event: the best we can say is SIGKILL.
		t.Logf("Docker emitted no oom event for this OOM kill")
		if term.Reason != "Killed" || term.ExitCode != 137 {
			t.Errorf("without an oom event the exit must read as a SIGKILL, got %+v", term)
		}
	}
}

func rawState(ctx context.Context, t *testing.T, project string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "-q", "--filter", "label=com.docker.compose.project="+project).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	id := strings.TrimSpace(string(out))
	out, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .State}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	var compact map[string]any
	if json.Unmarshal(out, &compact) == nil {
		delete(compact, "Health")
		b, _ := json.Marshal(compact)
		return string(b)
	}
	return string(out)
}

// rawEvents reports the container's die and oom events in log order,
// e.g. "oom@…221 die@…224 (oom before die)", so CI shows which order
// Docker used on each run.
func rawEvents(ctx context.Context, t *testing.T, project string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "-q", "--filter", "label=com.docker.compose.project="+project).Output()
	if err != nil {
		return "error: " + err.Error()
	}
	id := strings.TrimSpace(string(out))
	out, err = exec.CommandContext(ctx, "docker", "events",
		"--since", time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339),
		"--until", time.Now().UTC().Format(time.RFC3339Nano),
		"--filter", "container="+id,
		"--filter", "event=die", "--filter", "event=oom",
		"--format", "{{json .}}").Output()
	if err != nil {
		return "error: " + err.Error()
	}
	var parts []string
	var oomAt, dieAt int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var e dockerEvent
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s@%d", e.Action, e.TimeNano))
		switch e.Action {
		case "oom":
			oomAt = e.TimeNano
		case "die":
			if dieAt == 0 {
				dieAt = e.TimeNano
			}
		}
	}
	order := "no oom event"
	switch {
	case oomAt != 0 && oomAt <= dieAt:
		order = fmt.Sprintf("oom before die by %s", time.Duration(dieAt-oomAt))
	case oomAt != 0:
		order = fmt.Sprintf("oom AFTER die by %s", time.Duration(oomAt-dieAt))
	}
	return strings.Join(parts, " ") + " (" + order + ")"
}
