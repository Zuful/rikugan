//go:build integration

// These tests run against a real cluster (kind in CI) because the fake
// clientset has no Deployment controller: the whole point is to observe
// what the controller does to the Progressing condition and to the
// ReplicaSets, not what we assume it does.
//
// Run locally with a throwaway cluster:
//
//	kind create cluster
//	go test -tags integration -run Integration -v ./internal/deployments/...
package kubernetes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	// agnhost serves HTTP so the pods can carry a readiness probe. The
	// probe's initial delay keeps new pods unready for several seconds,
	// which forces the controller through intermediate "rollout in
	// progress" syncs on every scale-up and pod replacement. With pods
	// that are ready instantly (pause), those syncs never happen and a
	// rewrite of the Progressing condition would go unnoticed.
	image      = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	readyDelay = 8 // seconds
	versionA   = "a"
	versionB   = "b"

	pollInterval = time.Second
	pollTimeout  = 3 * time.Minute

	// Condition timestamps have one-second resolution. Waiting longer
	// than that before each mutation guarantees that a timestamp the
	// controller rewrites would differ from the previous one.
	settle = 2 * time.Second
)

func TestIntegration_RolloutTimestamps(t *testing.T) {
	cs := integrationClient(t)
	ctx := context.Background()
	ns := createNamespace(ctx, t, cs)
	const name = "probe"

	_, err := cs.AppsV1().Deployments(ns).Create(ctx, deployment(name, versionA, 1), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	base := waitComplete(ctx, t, cs, ns, name)
	t.Logf("initial rollout: %+v", base)
	if base.Rollback {
		t.Fatalf("initial rollout reported as rollback")
	}
	if base.CompletedAt.Before(base.StartedAt) {
		t.Fatalf("CompletedAt %s before StartedAt %s", base.CompletedAt, base.StartedAt)
	}

	// Each of these must leave the rollout untouched: same revision,
	// same StartedAt, same CompletedAt.
	//
	// slowReady marks the scenarios that create pods: they must be seen
	// going through a not-yet-available phase, otherwise the scenario did
	// not exercise the controller's in-progress syncs and proves nothing.
	unchanged := []struct {
		name      string
		replicas  int
		slowReady bool
		mutate    func(t *testing.T)
	}{
		{"scale 1 to 3", 3, true, func(t *testing.T) { scale(ctx, t, cs, ns, name, 3) }},
		{"replace a pod", 3, true, func(t *testing.T) { deleteOnePod(ctx, t, cs, ns, base.Revision) }},
		{"scale 3 to 1", 1, false, func(t *testing.T) { scale(ctx, t, cs, ns, name, 1) }},
		{"annotate deployment metadata", 1, false, func(t *testing.T) {
			patch(ctx, t, cs, ns, name, `{"metadata":{"annotations":{"probe/touched":"yes"}}}`)
		}},
	}
	for _, tc := range unchanged {
		t.Run(tc.name, func(t *testing.T) {
			time.Sleep(settle)
			tc.mutate(t)
			sawUnready := waitPodsReady(ctx, t, cs, ns, base.Revision, tc.replicas)
			if tc.slowReady && !sawUnready {
				t.Fatalf("no unready pod observed: the scenario did not exercise an in-progress sync")
			}
			got := waitComplete(ctx, t, cs, ns, name)
			t.Logf("after %s: %+v", tc.name, got)
			if got.Sequence != base.Sequence {
				t.Errorf("revision moved: %s -> %s", base.Sequence, got.Sequence)
			}
			if !got.StartedAt.Equal(base.StartedAt) {
				t.Errorf("StartedAt moved: %s -> %s", base.StartedAt, got.StartedAt)
			}
			if !got.CompletedAt.Equal(base.CompletedAt) {
				t.Errorf("CompletedAt moved: %s -> %s", base.CompletedAt, got.CompletedAt)
			}
		})
	}

	time.Sleep(settle)
	setVersion(ctx, t, cs, ns, name, versionB)
	upgrade := waitComplete(ctx, t, cs, ns, name)
	t.Logf("after template change: %+v", upgrade)
	if upgrade.Sequence == base.Sequence {
		t.Fatalf("template change did not produce a new revision")
	}
	if upgrade.Rollback {
		t.Errorf("template change reported as rollback")
	}
	if !upgrade.StartedAt.After(base.CompletedAt) {
		t.Errorf("new StartedAt %s not after previous CompletedAt %s", upgrade.StartedAt, base.CompletedAt)
	}
	if !upgrade.CompletedAt.After(base.CompletedAt) {
		t.Errorf("new CompletedAt %s not after previous %s", upgrade.CompletedAt, base.CompletedAt)
	}
	if upgrade.CompletedAt.Before(upgrade.StartedAt) {
		t.Errorf("CompletedAt %s before StartedAt %s", upgrade.CompletedAt, upgrade.StartedAt)
	}

	time.Sleep(settle)
	rollbackStart := time.Now().Add(-time.Second) // creationTimestamp is truncated to the second
	setVersion(ctx, t, cs, ns, name, versionA)
	rollback := waitComplete(ctx, t, cs, ns, name)
	t.Logf("after rollback: %+v", rollback)
	if !rollback.Rollback {
		t.Errorf("rollback to the first template not detected (no %s on the reused replicaset)", annotationRevisionHistory)
	}
	if rollback.Revision != base.Revision {
		t.Errorf("rollback did not reuse the first replicaset: template %s, want %s", rollback.Revision, base.Revision)
	}
	if rollback.StartedAt.Before(rollbackStart) {
		t.Errorf("rollback StartedAt %s predates the rollback (%s): reused replicaset's creation time leaked through", rollback.StartedAt, rollbackStart)
	}
	if !rollback.CompletedAt.After(upgrade.CompletedAt) {
		t.Errorf("rollback CompletedAt %s not after previous %s", rollback.CompletedAt, upgrade.CompletedAt)
	}
}

// A rollout whose new pods never become ready must be reported failed
// once the controller gives up (progressDeadlineSeconds), not left "in
// progress" forever: a stalled rollout is the worst case the health
// checks have to surface.
func TestIntegration_StalledRolloutIsFailed(t *testing.T) {
	cs := integrationClient(t)
	ctx := context.Background()
	ns := createNamespace(ctx, t, cs)
	const name = "stall"

	dep := deployment(name, versionA, 1)
	deadline := int32(15)
	dep.Spec.ProgressDeadlineSeconds = &deadline
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	good := waitComplete(ctx, t, cs, ns, name)

	time.Sleep(settle)
	// Same image, but the readiness probe targets a port nothing listens
	// on: the new ReplicaSet's pod runs and never becomes ready.
	patch(ctx, t, cs, ns, name, `{"spec":{"template":{"spec":{"containers":[{"name":"app","readinessProbe":{"httpGet":{"path":"/","port":9999}}}]}}}}`)

	// Until the controller creates the new ReplicaSet, the Deployment's
	// generation is ahead of what it has observed, so the previous
	// revision briefly reads in progress. Wait for the new one.
	inProgress := waitFor(ctx, t, cs, ns, name, "the new revision in progress", func(r provider.Rollout) bool {
		return r.State == provider.RolloutInProgress && r.Sequence != good.Sequence
	})
	t.Logf("while stalling: %+v", inProgress)
	if !inProgress.CompletedAt.IsZero() {
		t.Errorf("in-progress rollout has a completion time: %+v", inProgress)
	}

	failed := waitState(ctx, t, cs, ns, name, provider.RolloutFailed)
	t.Logf("after the progress deadline: %+v", failed)
	if failed.Sequence != inProgress.Sequence {
		t.Errorf("failed rollout is revision %s, want the stalled %s", failed.Sequence, inProgress.Sequence)
	}
	if failed.FailedAt.Before(failed.StartedAt) || !strings.HasPrefix(failed.FailureReason, reasonProgressDeadlineExceeded) {
		t.Errorf("failure not described: FailedAt %s StartedAt %s reason %q", failed.FailedAt, failed.StartedAt, failed.FailureReason)
	}
	if !failed.CompletedAt.IsZero() {
		t.Errorf("failed rollout has a completion time: %s", failed.CompletedAt)
	}
}

func integrationClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("home dir: %v", err)
		}
		path = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("load kubeconfig %s: %v", path, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return cs
}

func createNamespace(ctx context.Context, t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	ns, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "rollout-probe-"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return ns.Name
}

// deployment builds a Deployment whose pod template differs only by the
// PROBE_VERSION env var, so switching versions is a template change
// (new revision) and switching back is a rollback to the first
// ReplicaSet.
func deployment(name, version string, replicas int32) *appsv1.Deployment {
	sel := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: new(int64),
					Containers: []corev1.Container{{
						Name:  "app",
						Image: image,
						Args:  []string{"netexec", "--http-port=8080"},
						Env:   []corev1.EnvVar{{Name: "PROBE_VERSION", Value: version}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)},
							},
							InitialDelaySeconds: readyDelay,
							PeriodSeconds:       1,
						},
					}},
				},
			},
		},
	}
}

func waitComplete(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string) provider.Rollout {
	t.Helper()
	return waitState(ctx, t, cs, ns, name, provider.RolloutComplete)
}

func waitState(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string, want provider.RolloutState) provider.Rollout {
	t.Helper()
	return waitFor(ctx, t, cs, ns, name, string(want), func(r provider.Rollout) bool { return r.State == want })
}

func waitFor(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name, what string, done func(provider.Rollout) bool) provider.Rollout {
	t.Helper()
	p := New(cs, nil)
	var st provider.Rollout
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var err error
		st, err = p.LastRollout(ctx, provider.Service{Name: name, Namespace: ns})
		if err != nil {
			return false, nil // revision annotation or new replicaset not there yet
		}
		return done(st), nil
	})
	if err != nil {
		t.Fatalf("rollout of %s/%s never reached %s: %v (last state %+v)", ns, name, what, err, st)
	}
	return st
}

// waitPodsReady waits until exactly want live pods of the template exist
// and all are Ready, and reports whether it saw any of them unready on
// the way. Polling pods directly (rather than the Deployment status)
// rules out reading a status the controller has not updated yet.
func waitPodsReady(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, templateKey string, want int) (sawUnready bool) {
	t.Helper()
	sel := metav1.ListOptions{LabelSelector: labelPodTemplateHash + "=" + templateKey}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, pollTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, sel)
		if err != nil {
			return false, nil
		}
		live, ready := 0, 0
		for _, p := range pods.Items {
			if p.DeletionTimestamp != nil {
				continue
			}
			live++
			if podReady(&p) {
				ready++
			} else {
				sawUnready = true
			}
		}
		return live == want && ready == want, nil
	})
	if err != nil {
		t.Fatalf("pods of template %s never reached %d ready: %v", templateKey, want, err)
	}
	return sawUnready
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func scale(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string, replicas int32) {
	t.Helper()
	patch(ctx, t, cs, ns, name, fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
}

func setVersion(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name, version string) {
	t.Helper()
	patch(ctx, t, cs, ns, name, fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":"PROBE_VERSION","value":%q}]}]}}}}`, version))
}

func patch(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name, body string) {
	t.Helper()
	_, err := cs.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(body), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("patch %s/%s with %s: %v", ns, name, body, err)
	}
}

// deleteOnePod deletes a pod of the given template and waits until the
// ReplicaSet has created its replacement, so the following waitComplete
// observes the post-replacement state rather than the pre-deletion one.
func deleteOnePod(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, templateKey string) {
	t.Helper()
	sel := metav1.ListOptions{LabelSelector: labelPodTemplateHash + "=" + templateKey}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, sel)
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("list pods for template %s: %v (found %d)", templateKey, err, len(pods.Items))
	}
	victim := pods.Items[0]
	if err := cs.CoreV1().Pods(ns).Delete(ctx, victim.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod %s: %v", victim.Name, err)
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, sel)
		if err != nil {
			return false, nil
		}
		for _, p := range pods.Items {
			if p.Name != victim.Name && p.CreationTimestamp.After(victim.CreationTimestamp.Time) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("replacement for pod %s never appeared: %v", victim.Name, err)
	}
}
