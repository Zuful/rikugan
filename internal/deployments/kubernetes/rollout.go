// Package kubernetes is the Kubernetes-backed slice of the deployments
// concept: it reconstructs a Deployment's current rollout (which revision
// is serving, when its rollout started and when it completed) from
// cluster state alone, so a caller can anchor before/after comparison
// windows on the rollout rather than on "now".
package kubernetes

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Annotations and labels the Deployment controller maintains. They are
// part of the controller's observable contract (kubectl rollout relies
// on them), not internal details.
const (
	annotationRevision        = "deployment.kubernetes.io/revision"
	annotationRevisionHistory = "deployment.kubernetes.io/revision-history"
	labelPodTemplateHash      = "pod-template-hash"

	reasonNewReplicaSetAvailable = "NewReplicaSetAvailable"
)

// rolloutState is a Deployment's current rollout as reconstructed from
// the Deployment, its ReplicaSets and the new ReplicaSet's pods.
type rolloutState struct {
	Revision    string
	TemplateKey string // pod-template-hash of the new ReplicaSet
	Complete    bool
	Rollback    bool
	StartedAt   time.Time
	CompletedAt time.Time // zero while the rollout is in progress
}

// currentRollout reconstructs the rollout of the named Deployment.
//
// The new ReplicaSet is the owned ReplicaSet whose revision annotation
// matches the Deployment's. StartedAt is that ReplicaSet's creation
// time, except on a rollback: the controller then reuses an old
// ReplicaSet (and records its previous revisions in the
// revision-history annotation), so its creation time dates from the
// original deploy and the earliest of its current pods is used instead.
//
// CompletedAt is the lastUpdateTime of the Progressing condition once
// its reason is NewReplicaSetAvailable. Whether that timestamp stays put
// across scales and pod replacements is exactly what
// rollout_integration_test.go checks against a real controller.
func currentRollout(ctx context.Context, cs kubernetes.Interface, namespace, name string) (rolloutState, error) {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return rolloutState{}, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	revision := dep.Annotations[annotationRevision]
	if revision == "" {
		return rolloutState{}, fmt.Errorf("deployment %s/%s has no %s annotation yet", namespace, name, annotationRevision)
	}

	newRS, err := findNewReplicaSet(ctx, cs, dep, revision)
	if err != nil {
		return rolloutState{}, err
	}

	st := rolloutState{
		Revision:    revision,
		TemplateKey: newRS.Labels[labelPodTemplateHash],
		Rollback:    newRS.Annotations[annotationRevisionHistory] != "",
		StartedAt:   newRS.CreationTimestamp.Time,
		Complete:    rolloutComplete(dep),
	}

	if st.Rollback {
		earliest, err := earliestPodCreation(ctx, cs, namespace, st.TemplateKey)
		if err != nil {
			return rolloutState{}, err
		}
		if !earliest.IsZero() {
			st.StartedAt = earliest
		}
	}

	if st.Complete {
		for _, c := range dep.Status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Reason == reasonNewReplicaSetAvailable {
				st.CompletedAt = c.LastUpdateTime.Time
			}
		}
	}
	return st, nil
}

// rolloutComplete mirrors the condition kubectl rollout status waits for:
// the controller has observed the latest spec, every replica runs the new
// template, every one of them is available, and no old replica is left.
func rolloutComplete(dep *appsv1.Deployment) bool {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	s := dep.Status
	return s.ObservedGeneration >= dep.Generation &&
		s.UpdatedReplicas == want &&
		s.AvailableReplicas == want &&
		s.Replicas == want
}

func findNewReplicaSet(ctx context.Context, cs kubernetes.Interface, dep *appsv1.Deployment, revision string) (*appsv1.ReplicaSet, error) {
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s selector: %w", dep.Namespace, dep.Name, err)
	}
	list, err := cs.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list replicasets for %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	for i := range list.Items {
		rs := &list.Items[i]
		if !metav1.IsControlledBy(rs, dep) {
			continue
		}
		if rs.Annotations[annotationRevision] == revision {
			return rs, nil
		}
	}
	return nil, fmt.Errorf("no replicaset of %s/%s carries revision %s", dep.Namespace, dep.Name, revision)
}

func earliestPodCreation(ctx context.Context, cs kubernetes.Interface, namespace, templateKey string) (time.Time, error) {
	sel := labels.SelectorFromSet(labels.Set{labelPodTemplateHash: templateKey})
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return time.Time{}, fmt.Errorf("list pods for template %s: %w", templateKey, err)
	}
	var earliest time.Time
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		t := p.CreationTimestamp.Time
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest, nil
}
