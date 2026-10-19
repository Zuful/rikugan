package kubernetes

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Zuful/rikugan/pkg/provider"
)

// TestWatchRollouts drives the informer path on a fake clientset: a
// Deployment completes revision 2, then a stalled revision 3 fails.
func TestWatchRollouts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dep := fixtureDeployment("2", 3, 3, available(t0.Add(time.Minute)))
	cs := fake.NewClientset(dep, fixtureReplicaSet(dep, "two", "2", t0, ""))
	ch, err := New(cs, nil).WithNamespaces("prod").WatchRollouts(ctx)
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
			return r
		case <-ctx.Done():
			t.Fatalf("%s: nothing reported", what)
		}
		return provider.Rollout{}
	}

	r := next("initial state")
	if r.Sequence != "2" || r.State != provider.RolloutComplete || !r.CompletedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("initial: %+v", r)
	}

	// Revision 3: the ReplicaSet first, then the Deployment moving to it
	// and stalling.
	if _, err := cs.AppsV1().ReplicaSets("prod").Create(ctx, fixtureReplicaSet(dep, "three", "3", t0.Add(time.Hour), ""), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	moved := fixtureDeployment("3", 3, 1)
	if _, err := cs.AppsV1().Deployments("prod").Update(ctx, moved, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	stalledDep := fixtureDeployment("3", 3, 1, stalled(t0.Add(time.Hour+10*time.Minute)))
	if _, err := cs.AppsV1().Deployments("prod").Update(ctx, stalledDep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r = next("stalled revision")
	if r.Sequence != "3" || r.State != provider.RolloutFailed || r.Revision != "three" {
		t.Fatalf("stalled: %+v", r)
	}

	cancel()
	for range ch { // drained and closed on cancellation
	}
}

func TestWatchRolloutsDropsDeletedDeployments(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dep := fixtureDeployment("2", 1, 1, available(t0))
	cs := fake.NewClientset(dep, fixtureReplicaSet(dep, "two", "2", t0, ""))
	ch, err := New(cs, nil).WatchRollouts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-ch // initial complete
	if err := cs.AppsV1().Deployments("prod").Delete(ctx, "api", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Recreated under the same name, at revision 1 again. Whether or not
	// the deletion was processed first, the new rollout is reported and
	// the deleted one's history yields no superseded rollouts.
	again := fixtureDeployment("1", 1, 1, available(t0.Add(2*time.Hour)))
	again.UID = "dep-uid" // same owner UID as the fixture ReplicaSets
	if _, err := cs.AppsV1().ReplicaSets("prod").Create(ctx, fixtureReplicaSet(again, "one", "1", t0.Add(2*time.Hour), ""), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments("prod").Create(ctx, again, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		if r.Sequence != "1" || r.State != provider.RolloutComplete {
			t.Fatalf("after recreation: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("recreated deployment not reported")
	}
}
