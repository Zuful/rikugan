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

func TestIntegration_OOMKilledAfterRestart(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(oomCompose), 0o600); err != nil {
		t.Fatal(err)
	}
	const project = "rikugan-oom-probe"
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
	if term := inst.Terminations[0]; !term.OOM {
		t.Errorf("the first run was OOM-killed, but after the restart the instance reports %+v", term)
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
