package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestProvider returns a KubernetesProvider backed by a fake clientset.
func newTestProvider(pods ...*corev1.Pod) *KubernetesProvider {
	objs := make([]runtime.Object, len(pods))
	for i, pod := range pods {
		objs[i] = pod
	}
	cs := fake.NewClientset(objs...)
	return &KubernetesProvider{
		client: cs,
		logger: slog.Default(),
	}
}

func makePod(ns, name, phase string, containers []corev1.Container, statuses []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
		},
		Spec: corev1.PodSpec{
			Containers: containers,
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodPhase(phase),
			ContainerStatuses: statuses,
		},
	}
}

// ─── buildReport tests ──────────────────────────────────────────────────────

func TestBuildReport_BasicPod(t *testing.T) {
	p := newTestProvider()
	pod := makePod("default", "my-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:  "app",
			Ready: true,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
				},
			},
		}},
	)

	report := p.buildReport("default", "my-pod", pod, nil, map[string]string{"app": "some log line\n"})

	if !strings.Contains(report, "## Pod Diagnostic: default/my-pod") {
		t.Error("missing header in report")
	}
	if !strings.Contains(report, "Running") {
		t.Error("missing phase in report")
	}
	if !strings.Contains(report, "some log line") {
		t.Error("missing log content in report")
	}
}

func TestBuildReport_CrashLoopBackOff(t *testing.T) {
	p := newTestProvider()
	pod := makePod("default", "crash-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:         "app",
			Ready:        false,
			RestartCount: 5,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: "back-off 5m0s restarting failed container",
				},
			},
		}},
	)

	report := p.buildReport("default", "crash-pod", pod, nil, nil)

	if !strings.Contains(report, "CrashLoopBackOff") {
		t.Error("missing CrashLoopBackOff in report")
	}
	if !strings.Contains(report, "🔴") {
		t.Error("missing red emoji for CrashLoopBackOff")
	}
}

func TestBuildReport_Events(t *testing.T) {
	p := newTestProvider()
	pod := makePod("default", "my-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		nil,
	)

	events := []corev1.Event{
		{
			ObjectMeta:    metav1.ObjectMeta{Name: "evt1", Namespace: "default"},
			Type:          "Warning",
			Reason:        "BackOff",
			Message:       "Back-off restarting failed container",
			LastTimestamp: metav1.NewTime(time.Now().Add(-1 * time.Minute)),
		},
	}

	report := p.buildReport("default", "my-pod", pod, events, nil)

	if !strings.Contains(report, "BackOff") {
		t.Error("missing event reason in report")
	}
	if !strings.Contains(report, "Warning") {
		t.Error("missing event type in report")
	}
}

func TestBuildReport_ConditionsTable(t *testing.T) {
	p := newTestProvider()
	pod := makePod("default", "my-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		nil,
	)
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:    corev1.PodReady,
			Status:  corev1.ConditionTrue,
			Reason:  "PodReady",
			Message: "all containers ready",
		},
	}

	report := p.buildReport("default", "my-pod", pod, nil, nil)

	if !strings.Contains(report, "### Conditions") {
		t.Error("missing conditions section")
	}
	if !strings.Contains(report, "PodReady") {
		t.Error("missing condition reason")
	}
}

// ─── handleDiagnosePod integration tests ────────────────────────────────────

func makeCallToolRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "diagnose_pod",
			Arguments: args,
		},
	}
}

func TestHandleDiagnosePod_Success(t *testing.T) {
	pod := makePod("default", "test-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:  "app",
			Ready: true,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(time.Now()),
				},
			},
		}},
	)
	p := newTestProvider(pod)

	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "test-pod",
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Result should be a text content, not an error
	if result.IsError {
		t.Fatalf("expected success, got error result")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "test-pod") {
		t.Error("report should contain the pod name")
	}
}

func TestHandleDiagnosePod_PodNotFound(t *testing.T) {
	p := newTestProvider() // no pods

	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "nonexistent",
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for nonexistent pod")
	}
}

func TestHandleDiagnosePod_MissingNamespace(t *testing.T) {
	p := newTestProvider()

	req := makeCallToolRequest(map[string]any{
		"pod": "test-pod",
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing namespace")
	}
}

func TestHandleDiagnosePod_MissingPod(t *testing.T) {
	p := newTestProvider()

	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing pod name")
	}
}

// ─── Argument validation tests ──────────────────────────────────────────────

func TestHandleDiagnosePod_TailDefault(t *testing.T) {
	pod := makePod("default", "test-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		nil,
	)
	p := newTestProvider(pod)

	// No tail argument — should use default (50)
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "test-pod",
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success with default tail")
	}
}

func TestHandleDiagnosePod_TailCapped(t *testing.T) {
	pod := makePod("default", "test-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		nil,
	)
	p := newTestProvider(pod)

	// Tail > 1000 should be capped
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "test-pod",
		"tail":      5000,
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success with capped tail")
	}
}

func TestHandleDiagnosePod_TailNegative(t *testing.T) {
	pod := makePod("default", "test-pod", "Running",
		[]corev1.Container{{Name: "app"}},
		nil,
	)
	p := newTestProvider(pod)

	// Negative tail should fallback to default
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "test-pod",
		"tail":      -10,
	})

	result, err := p.handleDiagnosePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success with negative tail fallback")
	}
}

// ─── describeContainerState tests ───────────────────────────────────────────

func TestDescribeContainerState(t *testing.T) {
	tests := []struct {
		name      string
		status    corev1.ContainerStatus
		wantState string
	}{
		{
			name: "running",
			status: corev1.ContainerStatus{
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.NewTime(time.Now()),
					},
				},
			},
			wantState: "Running",
		},
		{
			name: "waiting",
			status: corev1.ContainerStatus{
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "image not found",
					},
				},
			},
			wantState: "Waiting",
		},
		{
			name: "terminated",
			status: corev1.ContainerStatus{
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Reason:   "Error",
					},
				},
			},
			wantState: "Terminated",
		},
		{
			name:      "unknown",
			status:    corev1.ContainerStatus{},
			wantState: "Unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := describeContainerState(tc.status)
			if state != tc.wantState {
				t.Errorf("got state %q, want %q", state, tc.wantState)
			}
		})
	}
}
