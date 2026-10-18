//go:build integration

package kubernetes

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Zuful/rikugan/pkg/provider"
)

// TestIntegration_WatchRolloutsBackToBack starts two rollouts of the same
// Deployment back to back, the second before the first can complete. The
// first must be reported superseded, never complete, and the second
// complete: whether the watcher saw the first in progress or only the
// jump from the initial revision to the last one (design v3.5, C3).
func TestIntegration_WatchRolloutsBackToBack(t *testing.T) {
	cs := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	ns := createNamespace(ctx, t, cs)
	const name = "burst"

	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, deployment(name, versionA, 1), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	initial := waitComplete(ctx, t, cs, ns, name)

	ch, err := New(cs, nil).WithNamespaces(ns).WatchRollouts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := func(what string) provider.Rollout {
		t.Helper()
		select {
		case r, ok := <-ch:
			if !ok {
				t.Fatalf("%s: channel closed", what)
			}
			t.Logf("reported: revision %s %s", r.Sequence, r.State)
			return r
		case <-ctx.Done():
			t.Fatalf("%s: nothing reported", what)
		}
		return provider.Rollout{}
	}
	if r := next("initial state"); r.Sequence != initial.Sequence || r.State != provider.RolloutComplete {
		t.Fatalf("initial: %+v, want revision %s complete", r, initial.Sequence)
	}

	// Two template changes in a row. The second waits only until the
	// controller has given the first its revision number (otherwise the
	// first would never exist as a rollout); its pods need readyDelay
	// seconds to be ready, so it cannot complete before the second
	// starts. The watcher may or may not have processed the first by
	// then; either way it must report it superseded.
	setVersion(ctx, t, cs, ns, name, versionB)
	waitFor(ctx, t, cs, ns, name, "the first rollout's revision", func(r provider.Rollout) bool {
		return r.Sequence != initial.Sequence
	})
	setVersion(ctx, t, cs, ns, name, "c")

	first := next("first rollout")
	if first.State != provider.RolloutSuperseded {
		t.Fatalf("first rollout reported %s, want superseded: %+v", first.State, first)
	}
	last := next("second rollout")
	if last.State != provider.RolloutComplete {
		t.Fatalf("second rollout reported %s, want complete: %+v", last.State, last)
	}
	if first.Sequence == last.Sequence || first.Sequence == initial.Sequence {
		t.Errorf("superseded revision %s, initial %s, last %s", first.Sequence, initial.Sequence, last.Sequence)
	}
	if last.CompletedAt.Before(last.StartedAt) {
		t.Errorf("last rollout: completed %s before it started %s", last.CompletedAt, last.StartedAt)
	}

	// Nothing else: in particular, no late "complete" for the first one.
	select {
	case r := <-ch:
		t.Errorf("unexpected report: revision %s %s", r.Sequence, r.State)
	case <-time.After(5 * time.Second):
	}
}
