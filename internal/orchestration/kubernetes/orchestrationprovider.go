package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.OrchestrationProvider —
// the concept-level Orchestration tools route through this backend.
var _ provider.OrchestrationProvider = (*Provider)(nil)

// kindDeployment / kindPod are the values this provider sets on
// provider.Service.Kind. The concept interface keeps Kind opaque to
// accommodate other orchestrators (Docker Compose uses
// kindDockerComposeService etc.).
const (
	kindDeployment = "deployment"
	kindPod        = "pod"
)

// ListServices returns every Deployment in the namespace. When namespace
// is empty all namespaces are scanned. The concept-level model treats a
// Deployment as a Service (a unit you can describe, restart, query
// history of). Bare pods are exposed via GetServiceState/Describe when
// the caller passes Kind="pod" explicitly — they aren't surfaced here
// because a one-off pod is rarely what the operator is looking for.
func (p *Provider) ListServices(ctx context.Context, namespace string) ([]provider.Service, error) {
	deps, err := p.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("kubernetes list deployments: %w", err)
	}
	out := make([]provider.Service, 0, len(deps.Items))
	for _, d := range deps.Items {
		out = append(out, deploymentToService(d))
	}
	return out, nil
}

// GetServiceState returns the current health of the given service. For
// a deployment, it reports desired vs. available replicas plus a
// status string derived from the deployment's conditions. For a pod,
// it reports phase and reason if waiting.
func (p *Provider) GetServiceState(ctx context.Context, svc provider.Service) (provider.ServiceState, error) {
	switch svc.Kind {
	case kindDeployment, "":
		d, err := p.client.AppsV1().Deployments(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if err != nil {
			return provider.ServiceState{}, fmt.Errorf("kubernetes get deployment: %w", err)
		}
		state := provider.ServiceState{
			Status:  deploymentStatus(d),
			Message: latestDeploymentConditionMessage(d),
		}
		state.Replicas.Desired = int(intPtrVal(d.Spec.Replicas, 1))
		state.Replicas.Available = int(d.Status.AvailableReplicas)
		return state, nil
	case kindPod:
		pod, err := p.client.CoreV1().Pods(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if err != nil {
			return provider.ServiceState{}, fmt.Errorf("kubernetes get pod: %w", err)
		}
		state := provider.ServiceState{
			Status:  podStatus(pod),
			Message: podStatusMessage(pod),
		}
		state.Replicas.Desired = 1
		if isPodReady(pod) {
			state.Replicas.Available = 1
		}
		return state, nil
	default:
		return provider.ServiceState{}, fmt.Errorf("kubernetes: unsupported service kind %q", svc.Kind)
	}
}

// DescribeService returns a markdown summary of the service's spec
// (image, replicas, labels) and current status (ready/total).
func (p *Provider) DescribeService(ctx context.Context, svc provider.Service) (string, error) {
	switch svc.Kind {
	case kindDeployment, "":
		d, err := p.client.AppsV1().Deployments(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("kubernetes get deployment: %w", err)
		}
		return describeDeployment(d), nil
	case kindPod:
		pod, err := p.client.CoreV1().Pods(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("kubernetes get pod: %w", err)
		}
		return describePod(pod), nil
	default:
		return "", fmt.Errorf("kubernetes: unsupported service kind %q", svc.Kind)
	}
}

// GetRecentEvents returns events involving the service's pods within
// the lookback window. For a deployment this includes events from
// every pod matched by its selector; for a single pod it's just the
// pod's own events.
func (p *Provider) GetRecentEvents(ctx context.Context, svc provider.Service, since time.Duration) ([]provider.Event, error) {
	if since <= 0 {
		since = time.Hour
	}
	cutoff := time.Now().Add(-since)

	var podNames []string
	switch svc.Kind {
	case kindDeployment, "":
		names, err := p.podNamesForDeployment(ctx, svc.Namespace, svc.Name)
		if err != nil {
			return nil, err
		}
		podNames = names
	case kindPod:
		podNames = []string{svc.Name}
	default:
		return nil, fmt.Errorf("kubernetes: unsupported service kind %q", svc.Kind)
	}

	if len(podNames) == 0 {
		return nil, nil
	}

	// Fetch events for the namespace once, filter client-side. The API's
	// fieldSelector only takes a single name and we may have several
	// pods to cover.
	evts, err := p.client.CoreV1().Events(svc.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("kubernetes list events: %w", err)
	}
	involved := make(map[string]bool, len(podNames))
	for _, n := range podNames {
		involved[n] = true
	}

	var out []provider.Event
	for _, e := range evts.Items {
		if !involved[e.InvolvedObject.Name] {
			continue
		}
		ts := lastSeenTime(e)
		if ts.Before(cutoff) {
			continue
		}
		out = append(out, provider.Event{
			Time:    ts,
			Type:    e.Type,
			Reason:  e.Reason,
			Message: e.Message,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// RestartService triggers a rolling restart. For a deployment that
// means patching the pod template with a kubectl.kubernetes.io/restartedAt
// annotation — the same mechanism `kubectl rollout restart` uses. For
// a bare pod it means deleting it (replication controller / replicaset
// will recreate it if one owns it).
func (p *Provider) RestartService(ctx context.Context, svc provider.Service) error {
	switch svc.Kind {
	case kindDeployment, "":
		patch := fmt.Sprintf(
			`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`,
			time.Now().UTC().Format(time.RFC3339),
		)
		_, err := p.client.AppsV1().Deployments(svc.Namespace).
			Patch(ctx, svc.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("kubernetes restart deployment: %w", err)
		}
		return nil
	case kindPod:
		if err := p.client.CoreV1().Pods(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("kubernetes delete pod: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("kubernetes: unsupported service kind %q", svc.Kind)
	}
}

// GetServiceHistory returns the deployment's rollout history — every
// ReplicaSet it has owned, sorted by revision descending. Bare pods
// have no revision history; the call returns an error in that case.
func (p *Provider) GetServiceHistory(ctx context.Context, svc provider.Service) (string, error) {
	if svc.Kind != kindDeployment && svc.Kind != "" {
		return "", fmt.Errorf("kubernetes: GetServiceHistory only supported for kind=deployment, got %q", svc.Kind)
	}
	d, err := p.client.AppsV1().Deployments(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("kubernetes get deployment: %w", err)
	}
	rsList, err := p.client.AppsV1().ReplicaSets(svc.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("kubernetes list replicasets: %w", err)
	}
	type revision struct {
		Revision string
		Created  time.Time
		Replicas int32
		Image    string
		Cause    string
	}
	var revs []revision
	for _, rs := range rsList.Items {
		if !ownedBy(rs.OwnerReferences, d.UID) {
			continue
		}
		rev := rs.Annotations["deployment.kubernetes.io/revision"]
		cause := rs.Annotations["kubernetes.io/change-cause"]
		img := firstContainerImage(rs.Spec.Template.Spec.Containers)
		revs = append(revs, revision{
			Revision: rev,
			Created:  rs.CreationTimestamp.Time,
			Replicas: rs.Status.Replicas,
			Image:    img,
			Cause:    cause,
		})
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].Revision > revs[j].Revision })

	var b strings.Builder
	fmt.Fprintf(&b, "## Deployment History — %s/%s\n\n", d.Namespace, d.Name)
	if len(revs) == 0 {
		b.WriteString("_No ReplicaSet history found for this deployment._\n")
		return b.String(), nil
	}
	b.WriteString("| Revision | Created | Replicas | Image | Change-cause |\n")
	b.WriteString("|----------|---------|----------|-------|--------------|\n")
	for _, r := range revs {
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n",
			r.Revision, r.Created.UTC().Format(time.RFC3339), r.Replicas, r.Image, fallback(r.Cause, "—"),
		)
	}
	return b.String(), nil
}

// ── Internal helpers ────────────────────────────────────────────────────────

func deploymentToService(d appsv1.Deployment) provider.Service {
	return provider.Service{
		Name:      d.Name,
		Namespace: d.Namespace,
		Kind:      kindDeployment,
		Labels:    cloneLabels(d.Labels),
	}
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func deploymentStatus(d *appsv1.Deployment) string {
	if d.Status.AvailableReplicas == intPtrVal(d.Spec.Replicas, 1) && d.Status.UnavailableReplicas == 0 {
		return "running"
	}
	if d.Status.AvailableReplicas == 0 {
		return "failed"
	}
	return "degraded"
}

func latestDeploymentConditionMessage(d *appsv1.Deployment) string {
	if len(d.Status.Conditions) == 0 {
		return ""
	}
	latest := d.Status.Conditions[0]
	for _, c := range d.Status.Conditions[1:] {
		if c.LastTransitionTime.After(latest.LastTransitionTime.Time) {
			latest = c
		}
	}
	return latest.Message
}

func podStatus(pod *corev1.Pod) string {
	switch pod.Status.Phase {
	case corev1.PodRunning:
		if isPodReady(pod) {
			return "running"
		}
		return "degraded"
	case corev1.PodSucceeded:
		return "running"
	case corev1.PodFailed:
		return "failed"
	case corev1.PodPending:
		return "pending"
	default:
		return "unknown"
	}
}

func podStatusMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Message != "" {
			return cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
		}
	}
	return string(pod.Status.Phase)
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func describeDeployment(d *appsv1.Deployment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Deployment %s/%s\n\n", d.Namespace, d.Name)
	fmt.Fprintf(&b, "| Field | Value |\n|-------|-------|\n")
	fmt.Fprintf(&b, "| Replicas (desired) | %d |\n", intPtrVal(d.Spec.Replicas, 1))
	fmt.Fprintf(&b, "| Replicas (available) | %d |\n", d.Status.AvailableReplicas)
	fmt.Fprintf(&b, "| Replicas (unavailable) | %d |\n", d.Status.UnavailableReplicas)
	fmt.Fprintf(&b, "| Strategy | %s |\n", d.Spec.Strategy.Type)
	fmt.Fprintf(&b, "| Created | %s |\n", d.CreationTimestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Containers | %s |\n", containerImages(d.Spec.Template.Spec.Containers))

	if len(d.Labels) > 0 {
		fmt.Fprintf(&b, "\n**Labels:**\n\n```json\n")
		raw, _ := json.MarshalIndent(d.Labels, "", "  ")
		b.Write(raw)
		b.WriteString("\n```\n")
	}

	if len(d.Status.Conditions) > 0 {
		b.WriteString("\n### Conditions\n\n| Type | Status | Reason | Message |\n|------|--------|--------|---------|\n")
		for _, c := range d.Status.Conditions {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return b.String()
}

func describePod(pod *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Pod %s/%s\n\n", pod.Namespace, pod.Name)
	fmt.Fprintf(&b, "| Field | Value |\n|-------|-------|\n")
	fmt.Fprintf(&b, "| Phase | %s |\n", pod.Status.Phase)
	fmt.Fprintf(&b, "| Node | %s |\n", pod.Spec.NodeName)
	fmt.Fprintf(&b, "| Created | %s |\n", pod.CreationTimestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Containers | %s |\n", containerImages(pod.Spec.Containers))

	if len(pod.Status.ContainerStatuses) > 0 {
		b.WriteString("\n### Container Statuses\n\n| Container | Ready | Restarts | State |\n|-----------|-------|----------|-------|\n")
		for _, cs := range pod.Status.ContainerStatuses {
			state, _ := containerStateString(cs)
			fmt.Fprintf(&b, "| %s | %v | %d | %s |\n", cs.Name, cs.Ready, cs.RestartCount, state)
		}
	}
	return b.String()
}

func containerStateString(cs corev1.ContainerStatus) (string, string) {
	switch {
	case cs.State.Running != nil:
		return "running", cs.State.Running.StartedAt.Format(time.RFC3339)
	case cs.State.Waiting != nil:
		return "waiting", cs.State.Waiting.Reason
	case cs.State.Terminated != nil:
		return "terminated", fmt.Sprintf("exit=%d reason=%s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
	default:
		return "unknown", ""
	}
}

func containerImages(cs []corev1.Container) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Image)
	}
	return strings.Join(parts, ", ")
}

func firstContainerImage(cs []corev1.Container) string {
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}

func intPtrVal(p *int32, fallback int32) int32 {
	if p == nil {
		return fallback
	}
	return *p
}

func lastSeenTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// podNamesForDeployment resolves the pod names currently selected by a
// deployment by walking its spec.selector.matchLabels.
func (p *Provider) podNamesForDeployment(ctx context.Context, namespace, name string) ([]string, error) {
	d, err := p.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("kubernetes get deployment: %w", err)
	}
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(d.Spec.Selector.MatchLabels))
	for k, v := range d.Spec.Selector.MatchLabels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	pods, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: strings.Join(parts, ",")})
	if err != nil {
		return nil, fmt.Errorf("kubernetes list pods: %w", err)
	}
	names := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		names = append(names, pod.Name)
	}
	return names, nil
}
