package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	annotationRevision   = "deployment.kubernetes.io/revision"
	labelPodTemplateHash = "pod-template-hash"
	reasonCrashLoop      = "CrashLoopBackOff"
	reasonOOMKilled      = "OOMKilled"
)

// GetRevisionInstances returns the live pods of one revision of a
// Deployment. revision is a pod-template-hash; when empty, the hash of
// the Deployment's current ReplicaSet is used. Pods that are being
// deleted are left out: they no longer serve the revision.
func (p *Provider) GetRevisionInstances(ctx context.Context, svc provider.Service, revision string) ([]provider.Instance, error) {
	dep, err := p.client.AppsV1().Deployments(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s selector: %w", svc.Namespace, svc.Name, err)
	}

	if revision == "" {
		revision, err = p.currentTemplateHash(ctx, svc.Namespace, sel, dep)
		if err != nil {
			return nil, err
		}
	}

	req, err := labels.NewRequirement(labelPodTemplateHash, "=", []string{revision})
	if err != nil {
		return nil, fmt.Errorf("revision %q: %w", revision, err)
	}
	pods, err := p.client.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel.Add(*req).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods of %s/%s revision %s: %w", svc.Namespace, svc.Name, revision, err)
	}

	out := make([]provider.Instance, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		out = append(out, podInstance(pod, revision))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// currentTemplateHash finds the ReplicaSet the Deployment controller
// considers current — the controlled one carrying the Deployment's
// revision annotation — and returns its pod-template-hash.
func (p *Provider) currentTemplateHash(ctx context.Context, namespace string, sel labels.Selector, dep metav1.Object) (string, error) {
	revision := dep.GetAnnotations()[annotationRevision]
	if revision == "" {
		return "", fmt.Errorf("deployment %s/%s has no %s annotation yet", namespace, dep.GetName(), annotationRevision)
	}
	rsList, err := p.client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return "", fmt.Errorf("list replicasets of %s/%s: %w", namespace, dep.GetName(), err)
	}
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if metav1.IsControlledBy(rs, dep) && rs.Annotations[annotationRevision] == revision {
			if hash := rs.Labels[labelPodTemplateHash]; hash != "" {
				return hash, nil
			}
		}
	}
	return "", fmt.Errorf("no replicaset of %s/%s carries revision %s", namespace, dep.GetName(), revision)
}

func podInstance(pod *corev1.Pod, revision string) provider.Instance {
	inst := provider.Instance{
		Name:     pod.Name,
		Revision: revision,
		Created:  pod.CreationTimestamp.Time,
	}

	// A pod with no Ready condition yet has never been ready: it has
	// been not-ready since it was created.
	inst.NotReadySince = pod.CreationTimestamp.Time
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodReady {
			continue
		}
		switch {
		case c.Status == corev1.ConditionTrue:
			inst.Ready = true
			inst.NotReadySince = time.Time{}
		case !c.LastTransitionTime.IsZero():
			inst.NotReadySince = c.LastTransitionTime.Time
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		inst.RestartCount += int(cs.RestartCount)
		if w := cs.State.Waiting; w != nil && w.Reason == reasonCrashLoop {
			inst.CrashLooping = true
		}
		// A container that is terminated right now has its latest stop
		// in State; otherwise the previous one is in LastTerminationState.
		term := cs.State.Terminated
		if term == nil {
			term = cs.LastTerminationState.Terminated
		}
		if term != nil {
			inst.Terminations = append(inst.Terminations, k8sTermination(cs.Name, term))
		}
	}
	return inst
}

func k8sTermination(container string, t *corev1.ContainerStateTerminated) provider.Termination {
	oom := t.Reason == reasonOOMKilled
	reason := t.Reason
	if reason == "" {
		reason = exitReason(int(t.ExitCode))
	}
	return provider.Termination{
		Container: container,
		Time:      t.FinishedAt.Time,
		Reason:    reason,
		ExitCode:  int(t.ExitCode),
		OOM:       oom,
	}
}

// exitReason names a termination the runtime left unlabelled. 137 is
// SIGKILL, which covers OOM kills but also evictions and forced stops,
// so it is "Killed", never "OOMKilled".
func exitReason(code int) string {
	switch code {
	case 0:
		return "Completed"
	case 137:
		return "Killed"
	default:
		return "Error"
	}
}
