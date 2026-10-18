package kubernetes

import (
	"context"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Zuful/rikugan/pkg/provider"
)

var riT0 = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func riDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "prod", UID: types.UID("dep-uid"),
			Annotations: map[string]string{annotationRevision: "4"},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		},
	}
}

func riReplicaSet(hash, revision string) *appsv1.ReplicaSet {
	ctrl := true
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-" + hash, Namespace: "prod",
		Labels:      map[string]string{"app": "api", labelPodTemplateHash: hash},
		Annotations: map[string]string{annotationRevision: revision},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "api", UID: "dep-uid", Controller: &ctrl,
		}},
	}}
}

type podOpt func(*corev1.Pod)

func riPod(name, hash string, created time.Time, opts ...podOpt) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "prod",
		Labels:            map[string]string{"app": "api", labelPodTemplateHash: hash},
		CreationTimestamp: metav1.NewTime(created),
	}}
	for _, o := range opts {
		o(p)
	}
	return p
}

func ready(status corev1.ConditionStatus, since time.Time) podOpt {
	return func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(since),
		})
	}
}

func container(cs corev1.ContainerStatus) podOpt {
	return func(p *corev1.Pod) { p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, cs) }
}

func riProvider(objs ...runtime.Object) *Provider {
	return &Provider{client: fake.NewClientset(objs...), logger: slog.Default()}
}

func TestGetRevisionInstances_DefaultsToCurrentRevision(t *testing.T) {
	p := riProvider(
		riDeployment(),
		riReplicaSet("old", "3"),
		riReplicaSet("new", "4"),
		riPod("api-old-1", "old", riT0.Add(-time.Hour), ready(corev1.ConditionTrue, riT0.Add(-time.Hour))),
		riPod("api-new-1", "new", riT0, ready(corev1.ConditionTrue, riT0.Add(10*time.Second))),
	)
	got, err := p.GetRevisionInstances(context.Background(), provider.Service{Name: "api", Namespace: "prod"}, "")
	if err != nil {
		t.Fatalf("GetRevisionInstances: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api-new-1" || got[0].Revision != "new" {
		t.Fatalf("want only api-new-1 of revision new, got %+v", got)
	}
	if !got[0].Ready || !got[0].NotReadySince.IsZero() {
		t.Errorf("ready pod: Ready=%v NotReadySince=%v", got[0].Ready, got[0].NotReadySince)
	}
}

func TestGetRevisionInstances_ExplicitRevisionAndDeletingPods(t *testing.T) {
	deleting := metav1.NewTime(riT0)
	p := riProvider(
		riDeployment(),
		riReplicaSet("new", "4"),
		riPod("api-old-1", "old", riT0.Add(-time.Hour)),
		riPod("api-old-2", "old", riT0.Add(-time.Hour), func(p *corev1.Pod) {
			p.DeletionTimestamp = &deleting
			p.Finalizers = []string{"test/keep"} // the fake tracker rejects a deletionTimestamp without finalizers
		}),
	)
	got, err := p.GetRevisionInstances(context.Background(), provider.Service{Name: "api", Namespace: "prod"}, "old")
	if err != nil {
		t.Fatalf("GetRevisionInstances: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api-old-1" {
		t.Fatalf("want only the live old pod, got %+v", got)
	}
}

func TestPodInstance(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want provider.Instance
	}{
		{
			name: "never ready yet: not ready since creation",
			pod:  riPod("p", "h", riT0),
			want: provider.Instance{Name: "p", Revision: "h", Created: riT0, NotReadySince: riT0},
		},
		{
			name: "lost readiness: not ready since the transition",
			pod:  riPod("p", "h", riT0, ready(corev1.ConditionFalse, riT0.Add(5*time.Minute))),
			want: provider.Instance{Name: "p", Revision: "h", Created: riT0, NotReadySince: riT0.Add(5 * time.Minute)},
		},
		{
			name: "attested OOM kill in the last termination",
			pod: riPod("p", "h", riT0, ready(corev1.ConditionTrue, riT0),
				container(corev1.ContainerStatus{
					Name: "app", RestartCount: 1,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(riT0.Add(time.Minute)),
					}},
				})),
			want: provider.Instance{
				Name: "p", Revision: "h", Created: riT0, Ready: true, RestartCount: 1,
				Terminations: []provider.Termination{{Container: "app", Time: riT0.Add(time.Minute), Reason: "OOMKilled", ExitCode: 137, OOM: true}},
			},
		},
		{
			name: "137 without OOMKilled reason is a kill, not an OOM",
			pod: riPod("p", "h", riT0, ready(corev1.ConditionTrue, riT0),
				container(corev1.ContainerStatus{
					Name: "app", RestartCount: 1,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 137, FinishedAt: metav1.NewTime(riT0.Add(time.Minute)),
					}},
				})),
			want: provider.Instance{
				Name: "p", Revision: "h", Created: riT0, Ready: true, RestartCount: 1,
				Terminations: []provider.Termination{{Container: "app", Time: riT0.Add(time.Minute), Reason: "Killed", ExitCode: 137}},
			},
		},
		{
			name: "crash loop summed over containers",
			pod: riPod("p", "h", riT0, ready(corev1.ConditionFalse, riT0.Add(time.Minute)),
				container(corev1.ContainerStatus{
					Name: "app", RestartCount: 3,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(riT0.Add(2 * time.Minute)),
					}},
				}),
				container(corev1.ContainerStatus{Name: "sidecar", RestartCount: 1})),
			want: provider.Instance{
				Name: "p", Revision: "h", Created: riT0, NotReadySince: riT0.Add(time.Minute), RestartCount: 4, CrashLooping: true,
				Terminations: []provider.Termination{{Container: "app", Time: riT0.Add(2 * time.Minute), Reason: "Error", ExitCode: 1}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := podInstance(tc.pod, "h")
			if !instancesEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func instancesEqual(a, b provider.Instance) bool {
	if a.Name != b.Name || a.Revision != b.Revision || !a.Created.Equal(b.Created) || a.Ready != b.Ready ||
		!a.NotReadySince.Equal(b.NotReadySince) || a.RestartCount != b.RestartCount || a.CrashLooping != b.CrashLooping ||
		len(a.Terminations) != len(b.Terminations) {
		return false
	}
	for i := range a.Terminations {
		x, y := a.Terminations[i], b.Terminations[i]
		if x.Container != y.Container || !x.Time.Equal(y.Time) || x.Reason != y.Reason || x.ExitCode != y.ExitCode || x.OOM != y.OOM {
			return false
		}
	}
	return true
}
