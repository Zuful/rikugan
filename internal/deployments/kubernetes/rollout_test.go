package kubernetes

import (
	"context"
	"reflect"
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

// These tests pin the reconstruction logic against hand-built cluster
// state. Whether the controller actually produces that state is covered
// by rollout_integration_test.go against a real cluster.

var t0 = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func fixtureDeployment(revision string, replicas, available int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "prod", UID: types.UID("dep-uid"), Generation: 4,
			Annotations: map[string]string{annotationRevision: revision},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "api:1"}},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 4,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			AvailableReplicas:  available,
			Conditions:         conds,
		},
	}
}

func fixtureReplicaSet(dep *appsv1.Deployment, hash, revision string, created time.Time, history string) *appsv1.ReplicaSet {
	ann := map[string]string{annotationRevision: revision}
	if history != "" {
		ann[annotationRevisionHistory] = history
	}
	ctrl := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-" + hash, Namespace: "prod",
			Labels:            map[string]string{"app": "api", labelPodTemplateHash: hash},
			Annotations:       ann,
			CreationTimestamp: metav1.NewTime(created),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: &ctrl,
			}},
		},
	}
}

func fixturePod(name, hash string, created time.Time) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "prod",
		Labels:            map[string]string{"app": "api", labelPodTemplateHash: hash},
		CreationTimestamp: metav1.NewTime(created),
	}}
}

func available(at time.Time) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue,
		Reason: reasonNewReplicaSetAvailable, LastUpdateTime: metav1.NewTime(at),
	}
}

func stalled(at time.Time) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: reasonProgressDeadlineExceeded, Message: `ReplicaSet "api-new" has timed out progressing.`,
		LastUpdateTime: metav1.NewTime(at),
	}
}

func svc() provider.Service {
	return provider.Service{Name: "api", Namespace: "prod", Kind: "deployment"}
}

func TestLastRollout(t *testing.T) {
	tests := []struct {
		name    string
		objects func() []runtime.Object
		want    provider.Rollout
	}{
		{
			name: "completed rollout to a new replicaset",
			objects: func() []runtime.Object {
				dep := fixtureDeployment("2", 3, 3, available(t0.Add(90*time.Second)))
				return []runtime.Object{
					dep,
					fixtureReplicaSet(dep, "old", "1", t0.Add(-24*time.Hour), ""),
					fixtureReplicaSet(dep, "new", "2", t0, ""),
				}
			},
			want: provider.Rollout{Revision: "new", Sequence: "2", State: provider.RolloutComplete, StartedAt: t0, CompletedAt: t0.Add(90 * time.Second)},
		},
		{
			name: "rollout in progress has no completion time",
			objects: func() []runtime.Object {
				dep := fixtureDeployment("2", 3, 1)
				return []runtime.Object{dep, fixtureReplicaSet(dep, "new", "2", t0, "")}
			},
			want: provider.Rollout{Revision: "new", Sequence: "2", State: provider.RolloutInProgress, StartedAt: t0},
		},
		{
			name: "stalled rollout past its progress deadline is failed",
			objects: func() []runtime.Object {
				dep := fixtureDeployment("2", 3, 1, stalled(t0.Add(10*time.Minute)))
				return []runtime.Object{dep, fixtureReplicaSet(dep, "new", "2", t0, "")}
			},
			want: provider.Rollout{
				Revision: "new", Sequence: "2", State: provider.RolloutFailed, StartedAt: t0,
				FailedAt: t0.Add(10 * time.Minute), FailureReason: `ProgressDeadlineExceeded: ReplicaSet "api-new" has timed out progressing.`,
			},
		},
		{
			name: "rollback reuses an old replicaset and starts at its newest pods",
			objects: func() []runtime.Object {
				dep := fixtureDeployment("3", 2, 2, available(t0.Add(70*time.Second)))
				return []runtime.Object{
					dep,
					fixtureReplicaSet(dep, "first", "3", t0.Add(-48*time.Hour), "1"),
					fixtureReplicaSet(dep, "second", "2", t0.Add(-24*time.Hour), ""),
					fixturePod("api-first-b", "first", t0.Add(20*time.Second)),
					fixturePod("api-first-a", "first", t0.Add(5*time.Second)),
				}
			},
			want: provider.Rollout{Revision: "first", Sequence: "3", Rollback: true, State: provider.RolloutComplete, StartedAt: t0.Add(5 * time.Second), CompletedAt: t0.Add(70 * time.Second)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New(fake.NewClientset(tc.objects()...), nil)
			got, err := p.LastRollout(context.Background(), svc())
			if err != nil {
				t.Fatalf("LastRollout: %v", err)
			}
			tc.want.Service = svc()
			tc.want.Images = []string{"api:1"}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestLastRollout_IgnoresReplicaSetsOwnedByAnotherDeployment(t *testing.T) {
	dep := fixtureDeployment("2", 1, 1, available(t0))
	other := fixtureDeployment("2", 1, 1)
	other.Name, other.UID = "api-canary", "other-uid"
	p := New(fake.NewClientset(dep, fixtureReplicaSet(other, "canary", "2", t0, "")), nil)

	if _, err := p.LastRollout(context.Background(), svc()); err == nil {
		t.Fatal("expected an error: the only revision-2 replicaset belongs to another deployment")
	}
}
