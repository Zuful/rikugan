//go:build integration

// This test runs against a real Docker daemon with Docker Compose: the
// stubbed runner cannot say whether Compose really recreates containers
// with a new config-hash, removes the old ones before the new are
// ready, or how the healthcheck log looks right after a start.
//
// Run locally:
//
//	go test -tags integration -run Integration -v ./internal/deployments/dockercompose/...
package dockercompose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	orchdc "github.com/Zuful/rikugan/internal/orchestration/dockercompose"
	"github.com/Zuful/rikugan/pkg/provider"
)

// The service is ready once its healthcheck passes, unless HEALTH=false.
// Changing VERSION or HEALTH changes the config-hash: a new revision.
const composeFile = `services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "exec sleep 3600"]
    environment:
      VERSION: ${VERSION}
      HEALTH: ${HEALTH}
    healthcheck:
      test: ["CMD", "sh", "-c", "[ \"$$HEALTH\" != false ]"]
      interval: 1s
      timeout: 1s
      retries: 1
`

func TestIntegration_ComposeRollouts(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const project = "rikugan-rollout-probe"
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte(composeFile), 0o600); err != nil {
		t.Fatal(err)
	}
	up := func(version, health string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "docker", "compose", "-p", project, "-f", file, "up", "-d")
		cmd.Env = append(os.Environ(), "VERSION="+version, "HEALTH="+health)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("compose up %s: %v\n%s", version, err, out)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", project, "-f", file, "down", "-t", "0").Run()
	})

	orch := orchdc.New(orchdc.WithProject(project))
	p := New(orch, WithProjects(project), WithRolloutDeadline(20*time.Second), WithPollInterval(time.Second))
	svc := provider.Service{Name: "app", Namespace: project}

	up("1", "true")
	first := waitState(ctx, t, p, svc, "", provider.RolloutComplete)
	t.Logf("first revision: %s started %s, completed %s", first.Sequence, first.StartedAt.Format(time.RFC3339Nano), first.CompletedAt.Format(time.RFC3339Nano))

	events, err := p.WatchRollouts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := func(what string) provider.Rollout {
		t.Helper()
		select {
		case r := <-events:
			t.Logf("reported: %s %s", r.Sequence, r.State)
			return r
		case <-ctx.Done():
			t.Fatalf("%s: nothing reported", what)
		}
		return provider.Rollout{}
	}
	if r := next("initial"); r.Revision != first.Revision || r.State != provider.RolloutComplete {
		t.Fatalf("initial: %+v", r)
	}

	// A new configuration: Compose recreates the container with a new
	// config-hash.
	up("2", "true")
	second := next("second revision")
	if second.State != provider.RolloutComplete || second.Revision == first.Revision {
		t.Fatalf("second revision: %+v (first %s)", second, first.Revision)
	}
	if second.StartedAt.Before(first.CompletedAt) || second.CompletedAt.Before(second.StartedAt) {
		t.Errorf("second revision started %s, completed %s; first completed %s",
			second.StartedAt.Format(time.RFC3339Nano), second.CompletedAt.Format(time.RFC3339Nano), first.CompletedAt.Format(time.RFC3339Nano))
	}
	if d := second.CompletedAt.Sub(second.StartedAt); d > 15*time.Second {
		t.Errorf("readiness took %s by the rollout's dates; the healthcheck passes within seconds", d)
	}

	// A revision that never becomes healthy: failed at the deadline.
	up("3", "false")
	third := next("unhealthy revision")
	if third.State != provider.RolloutFailed || third.Revision == second.Revision {
		t.Fatalf("unhealthy revision: %+v", third)
	}
	if got := third.FailedAt.Sub(third.StartedAt); got != 20*time.Second {
		t.Errorf("failed %s after its start, want the 20 s deadline", got)
	}

	// Fixed by a new configuration: the fix-forward completes; the
	// failed revision is not reported superseded.
	up("4", "true")
	fourth := next("fix-forward")
	if fourth.State != provider.RolloutComplete || fourth.Revision == third.Revision {
		t.Fatalf("fix-forward: %+v", fourth)
	}
}

func waitState(ctx context.Context, t *testing.T, p *Provider, svc provider.Service, notRevision string, want provider.RolloutState) provider.Rollout {
	t.Helper()
	for {
		r, err := p.LastRollout(ctx, svc)
		if err == nil && r.State == want && r.Revision != notRevision {
			return r
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never reached %s: last %+v, %v", want, r, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
