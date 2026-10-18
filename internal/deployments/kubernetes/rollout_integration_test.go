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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	imageA = "registry.k8s.io/pause:3.10"
	imageB = "registry.k8s.io/pause:3.9"

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

	_, err := cs.AppsV1().Deployments(ns).Create(ctx, deployment(name, imageA, 1), metav1.CreateOptions{})
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
	unchanged := []struct {
		name   string
		mutate func(t *testing.T)
	}{
		{"scale 1 to 3", func(t *testing.T) { scale(ctx, t, cs, ns, name, 3) }},
		{"scale 3 to 1", func(t *testing.T) { scale(ctx, t, cs, ns, name, 1) }},
		{"annotate deployment metadata", func(t *testing.T) {
			patch(ctx, t, cs, ns, name, `{"metadata":{"annotations":{"probe/touched":"yes"}}}`)
		}},
		{"replace a pod", func(t *testing.T) { deleteOnePod(ctx, t, cs, ns, base.TemplateKey) }},
	}
	for _, tc := range unchanged {
		t.Run(tc.name, func(t *testing.T) {
			time.Sleep(settle)
			tc.mutate(t)
			got := waitComplete(ctx, t, cs, ns, name)
			t.Logf("after %s: %+v", tc.name, got)
			if got.Revision != base.Revision {
				t.Errorf("revision moved: %s -> %s", base.Revision, got.Revision)
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
	setImage(ctx, t, cs, ns, name, imageB)
	upgrade := waitComplete(ctx, t, cs, ns, name)
	t.Logf("after image change: %+v", upgrade)
	if upgrade.Revision == base.Revision {
		t.Fatalf("image change did not produce a new revision")
	}
	if upgrade.Rollback {
		t.Errorf("image change reported as rollback")
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
	setImage(ctx, t, cs, ns, name, imageA)
	rollback := waitComplete(ctx, t, cs, ns, name)
	t.Logf("after rollback: %+v", rollback)
	if !rollback.Rollback {
		t.Errorf("rollback to the first template not detected (no %s on the reused replicaset)", annotationRevisionHistory)
	}
	if rollback.TemplateKey != base.TemplateKey {
		t.Errorf("rollback did not reuse the first replicaset: template %s, want %s", rollback.TemplateKey, base.TemplateKey)
	}
	if rollback.StartedAt.Before(rollbackStart) {
		t.Errorf("rollback StartedAt %s predates the rollback (%s): reused replicaset's creation time leaked through", rollback.StartedAt, rollbackStart)
	}
	if !rollback.CompletedAt.After(upgrade.CompletedAt) {
		t.Errorf("rollback CompletedAt %s not after previous %s", rollback.CompletedAt, upgrade.CompletedAt)
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

func deployment(name, image string, replicas int32) *appsv1.Deployment {
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
					Containers:                    []corev1.Container{{Name: "pause", Image: image}},
				},
			},
		},
	}
}

func waitComplete(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string) rolloutState {
	t.Helper()
	var st rolloutState
	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		var err error
		st, err = currentRollout(ctx, cs, ns, name)
		if err != nil {
			return false, nil // revision annotation or new replicaset not there yet
		}
		return st.Complete && !st.CompletedAt.IsZero(), nil
	})
	if err != nil {
		t.Fatalf("rollout of %s/%s did not complete: %v (last state %+v)", ns, name, err, st)
	}
	return st
}

func scale(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string, replicas int32) {
	t.Helper()
	patch(ctx, t, cs, ns, name, fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
}

func setImage(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name, image string) {
	t.Helper()
	patch(ctx, t, cs, ns, name, fmt.Sprintf(`{"spec":{"template":{"spec":{"containers":[{"name":"pause","image":%q}]}}}}`, image))
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
