// Package kubernetes is the Kubernetes-backed slice of the deployments
// concept: it reconstructs a Deployment's current rollout (which revision
// is serving or being rolled out, when the rollout started, and whether
// and when it completed or failed) from cluster state alone, so a caller
// can anchor before/after comparison windows on the rollout rather than
// on "now".
package kubernetes

import (
	"context"
	"fmt"
	"log/slog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.DeploymentSource.
var _ provider.DeploymentSource = (*Provider)(nil)

// Annotations, labels and condition reasons the Deployment controller
// maintains. They are part of the controller's observable contract
// (kubectl rollout relies on them), not internal details.
const (
	annotationRevision        = "deployment.kubernetes.io/revision"
	annotationRevisionHistory = "deployment.kubernetes.io/revision-history"
	labelPodTemplateHash      = "pod-template-hash"

	reasonNewReplicaSetAvailable   = "NewReplicaSetAvailable"
	reasonProgressDeadlineExceeded = "ProgressDeadlineExceeded"
)

// Provider is the Kubernetes DeploymentSource. It registers no MCP tool
// of its own: its rollouts feed the health checks of the correlation
// concept.
type Provider struct {
	client kubernetes.Interface
	logger *slog.Logger
}

// New builds a Provider on an existing clientset.
func New(client kubernetes.Interface, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{client: client, logger: logger}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "deployments/kubernetes" }

// Tools implements provider.Provider.
func (p *Provider) Tools() []provider.ToolDefinition { return nil }

// LastRollout returns the rollout of the Deployment named by svc.
//
// The new ReplicaSet is the owned ReplicaSet whose revision annotation
// matches the Deployment's. StartedAt is that ReplicaSet's creation
// time, except on a rollback: the controller then reuses an old
// ReplicaSet (and records its previous revisions in the
// revision-history annotation), so its creation time dates from the
// original deploy and the earliest of its current pods is used instead.
//
// CompletedAt is the lastUpdateTime of the Progressing condition once
// its reason is NewReplicaSetAvailable and the rollout is complete; the
// kind test in rollout_integration_test.go showed it does not move on
// scales, pod replacements or metadata patches. A Progressing condition
// with status False and reason ProgressDeadlineExceeded marks the
// rollout failed: the controller has given up waiting for the new
// ReplicaSet to become available.
func (p *Provider) LastRollout(ctx context.Context, svc provider.Service) (provider.Rollout, error) {
	dep, err := p.client.AppsV1().Deployments(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return provider.Rollout{}, fmt.Errorf("get deployment %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	sequence := dep.Annotations[annotationRevision]
	if sequence == "" {
		return provider.Rollout{}, fmt.Errorf("deployment %s/%s has no %s annotation yet", svc.Namespace, svc.Name, annotationRevision)
	}

	newRS, err := p.findNewReplicaSet(ctx, dep, sequence)
	if err != nil {
		return provider.Rollout{}, err
	}

	r := provider.Rollout{
		Service:   svc,
		Revision:  newRS.Labels[labelPodTemplateHash],
		Sequence:  sequence,
		Images:    templateImages(dep),
		Rollback:  newRS.Annotations[annotationRevisionHistory] != "",
		State:     provider.RolloutInProgress,
		StartedAt: newRS.CreationTimestamp.Time,
	}

	if r.Rollback {
		earliest, err := p.earliestPodCreation(ctx, dep.Namespace, r.Revision)
		if err != nil {
			return provider.Rollout{}, err
		}
		if !earliest.IsZero() {
			r.StartedAt = earliest.Time
		}
	}

	progressing := progressingCondition(dep)
	switch {
	case rolloutComplete(dep) && progressing != nil && progressing.Reason == reasonNewReplicaSetAvailable:
		r.State = provider.RolloutComplete
		r.CompletedAt = progressing.LastUpdateTime.Time
	case progressing != nil && progressing.Status == corev1.ConditionFalse && progressing.Reason == reasonProgressDeadlineExceeded:
		r.State = provider.RolloutFailed
		r.FailedAt = progressing.LastUpdateTime.Time
		r.FailureReason = progressing.Reason + ": " + progressing.Message
	}
	return r, nil
}

func progressingCondition(dep *appsv1.Deployment) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == appsv1.DeploymentProgressing {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
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

func templateImages(dep *appsv1.Deployment) []string {
	var images []string
	for _, c := range dep.Spec.Template.Spec.Containers {
		images = append(images, c.Image)
	}
	return images
}

func (p *Provider) findNewReplicaSet(ctx context.Context, dep *appsv1.Deployment, revision string) (*appsv1.ReplicaSet, error) {
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s selector: %w", dep.Namespace, dep.Name, err)
	}
	list, err := p.client.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
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

func (p *Provider) earliestPodCreation(ctx context.Context, namespace, templateKey string) (earliest metav1.Time, err error) {
	sel := labels.SelectorFromSet(labels.Set{labelPodTemplateHash: templateKey})
	pods, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return metav1.Time{}, fmt.Errorf("list pods for template %s: %w", templateKey, err)
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if earliest.IsZero() || pod.CreationTimestamp.Before(&earliest) {
			earliest = pod.CreationTimestamp
		}
	}
	return earliest, nil
}
