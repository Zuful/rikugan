package runes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/common/model"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// ─── Mock Prometheus API ─────────────────────────────────────────────────────

type mockAPI struct {
	promv1.API
	responses map[string]mockResponse
}

type mockResponse struct {
	value model.Value
	err   error
}

func (m *mockAPI) QueryRange(_ context.Context, query string, _ promv1.Range, _ ...promv1.Option) (model.Value, promv1.Warnings, promv1.Infos, error) {
	if resp, ok := m.responses[query]; ok {
		return resp.value, nil, nil, resp.err
	}
	// Default: return empty matrix
	return model.Matrix{}, nil, nil, nil
}

// ─── Test helpers ────────────────────────────────────────────────────────────

func newTestProvider(pods []*corev1.Pod, promResponses map[string]mockResponse) *Provider {
	objs := make([]runtime.Object, len(pods))
	for i, pod := range pods {
		objs[i] = pod
	}
	cs := fake.NewClientset(objs...)
	api := &mockAPI{responses: promResponses}
	return &Provider{
		k8s:    cs,
		prom:   api,
		logger: slog.Default(),
	}
}

func makePod(ns, name string, phase corev1.PodPhase, containers []corev1.Container, statuses []corev1.ContainerStatus) *corev1.Pod {
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
			Phase:             phase,
			ContainerStatuses: statuses,
		},
	}
}

func makeCallToolRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "six_eyes",
			Arguments: args,
		},
	}
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("empty result content")
	}
	return result.Content[0].(mcp.TextContent).Text
}

// ─── Argument validation tests ───────────────────────────────────────────────

func TestHandleSixEyes_MissingNamespace(t *testing.T) {
	p := newTestProvider(nil, nil)
	req := makeCallToolRequest(map[string]any{"pod": "test"})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing namespace")
	}
}

func TestHandleSixEyes_MissingPod(t *testing.T) {
	p := newTestProvider(nil, nil)
	req := makeCallToolRequest(map[string]any{"namespace": "default"})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing pod")
	}
}

func TestHandleSixEyes_InvalidWindow(t *testing.T) {
	p := newTestProvider(nil, nil)
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "test",
		"window":    "not-a-duration",
	})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for invalid window")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "invalid window") {
		t.Errorf("unexpected error: %s", text)
	}
}

// ─── OOMKill scenario ────────────────────────────────────────────────────────

func TestHandleSixEyes_OOMKill(t *testing.T) {
	pod := makePod("default", "oom-pod", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:         "app",
			Ready:        false,
			RestartCount: 3,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
		}},
	)

	p := newTestProvider([]*corev1.Pod{pod}, nil)
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "oom-pod",
	})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	if !strings.Contains(text, "OOM_KILL") {
		t.Error("expected OOM_KILL finding in report")
	}
	if !strings.Contains(text, "CRITICAL") {
		t.Error("expected CRITICAL severity")
	}
}

// ─── Application crash scenario ──────────────────────────────────────────────

func TestHandleSixEyes_ApplicationCrash(t *testing.T) {
	pod := makePod("default", "crash-pod", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:         "app",
			Ready:        false,
			RestartCount: 2,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
				},
			},
		}},
	)

	p := newTestProvider([]*corev1.Pod{pod}, nil)
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "crash-pod",
	})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error")
	}

	text := resultText(t, result)
	if !strings.Contains(text, "APPLICATION_CRASH") {
		t.Error("expected APPLICATION_CRASH finding")
	}
	if !strings.Contains(text, "HIGH") {
		t.Error("expected HIGH severity")
	}
}

// ─── Healthy pod scenario ────────────────────────────────────────────────────

func TestHandleSixEyes_Healthy(t *testing.T) {
	pod := makePod("default", "healthy-pod", corev1.PodRunning,
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

	p := newTestProvider([]*corev1.Pod{pod}, nil)
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "healthy-pod",
	})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error")
	}

	text := resultText(t, result)
	if !strings.Contains(text, "HEALTHY") {
		t.Error("expected HEALTHY finding")
	}
	if !strings.Contains(text, "INFO") {
		t.Error("expected INFO severity")
	}
}

// ─── Pod not found ───────────────────────────────────────────────────────────

func TestHandleSixEyes_PodNotFound(t *testing.T) {
	p := newTestProvider(nil, nil) // no pods

	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "nonexistent",
	})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for nonexistent pod")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "get pod") {
		t.Errorf("expected pod error message, got: %s", text)
	}
}

// ─── Partial Prometheus failure ──────────────────────────────────────────────

func TestHandleSixEyes_PartialPrometheusFailure(t *testing.T) {
	pod := makePod("default", "test-pod", corev1.PodRunning,
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

	cpuQuery := `rate(container_cpu_usage_seconds_total{namespace="default",pod="test-pod",container!=""}[5m])`
	responses := map[string]mockResponse{
		cpuQuery: {err: fmt.Errorf("connection refused")},
	}

	p := newTestProvider([]*corev1.Pod{pod}, responses)
	req := makeCallToolRequest(map[string]any{
		"namespace": "default",
		"pod":       "test-pod",
	})

	result, err := p.handleSixEyes(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success even with partial failure, got error")
	}

	text := resultText(t, result)
	if !strings.Contains(text, "connection refused") {
		t.Error("expected inline error for failed query")
	}
	// Report should still be generated
	if !strings.Contains(text, "## Six Eyes Correlation") {
		t.Error("expected report header")
	}
}

// ─── buildQueries unit tests ─────────────────────────────────────────────────

func TestBuildQueries_BaseOnly(t *testing.T) {
	pod := makePod("default", "test-pod", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:  "app",
			Ready: true,
		}},
	)

	now := time.Now()
	queries := buildQueries("default", "test-pod", pod, now.Add(-time.Hour), now)

	if len(queries) != 3 {
		t.Fatalf("expected 3 base queries, got %d", len(queries))
	}
}

func TestBuildQueries_WithRestartsAddsConditional(t *testing.T) {
	pod := makePod("default", "test-pod", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:         "app",
			RestartCount: 5,
		}},
	)

	now := time.Now()
	queries := buildQueries("default", "test-pod", pod, now.Add(-time.Hour), now)

	if len(queries) != 6 {
		t.Fatalf("expected 6 queries (3 base + 3 conditional), got %d", len(queries))
	}

	// Check conditional query names
	names := make(map[string]bool)
	for _, q := range queries {
		names[q.Name] = true
	}
	for _, expected := range []string{"OOMKill Events", "Memory Limits", "CPU Throttling"} {
		if !names[expected] {
			t.Errorf("missing expected conditional query: %s", expected)
		}
	}
}

func TestBuildQueries_PendingAddsNodePressure(t *testing.T) {
	pod := makePod("default", "test-pod", corev1.PodPending,
		[]corev1.Container{{Name: "app"}},
		nil,
	)

	now := time.Now()
	queries := buildQueries("default", "test-pod", pod, now.Add(-time.Hour), now)

	if len(queries) != 4 {
		t.Fatalf("expected 4 queries (3 base + 1 node pressure), got %d", len(queries))
	}

	last := queries[len(queries)-1]
	if last.Name != "Node Pressure" {
		t.Errorf("expected last query to be Node Pressure, got %s", last.Name)
	}
}

// ─── correlate unit tests ────────────────────────────────────────────────────

func TestCorrelate_OOMKill(t *testing.T) {
	pod := makePod("default", "test", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name: "app",
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
		}},
	)

	findings := correlate(
		podSnapshot{Pod: pod},
		metricSnapshot{},
	)

	if len(findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	if findings[0].Signal != "OOM_KILL" {
		t.Errorf("expected OOM_KILL, got %s", findings[0].Signal)
	}
	if findings[0].Severity != CRITICAL {
		t.Errorf("expected CRITICAL, got %s", findings[0].Severity)
	}
}

func TestCorrelate_ApplicationCrash(t *testing.T) {
	pod := makePod("default", "test", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name: "app",
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
				},
			},
		}},
	)

	findings := correlate(
		podSnapshot{Pod: pod},
		metricSnapshot{},
	)

	if len(findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	if findings[0].Signal != "APPLICATION_CRASH" {
		t.Errorf("expected APPLICATION_CRASH, got %s", findings[0].Signal)
	}
	if findings[0].Severity != HIGH {
		t.Errorf("expected HIGH, got %s", findings[0].Severity)
	}
}

func TestCorrelate_CPUStarvation(t *testing.T) {
	pod := makePod("default", "test", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:         "app",
			RestartCount: 3,
		}},
	)

	now := time.Now()
	ms := metricSnapshot{
		Results: []MetricResult{{
			Query: MetricQuery{Name: "CPU Throttling"},
			Matrix: model.Matrix{
				{
					Metric: model.Metric{},
					Values: []model.SamplePair{
						{Timestamp: model.TimeFromUnix(now.Unix()), Value: 0.5},
					},
				},
			},
		}},
	}

	findings := correlate(podSnapshot{Pod: pod}, ms)

	if len(findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	if findings[0].Signal != "CPU_STARVATION" {
		t.Errorf("expected CPU_STARVATION, got %s", findings[0].Signal)
	}
}

func TestCorrelate_NodePressure(t *testing.T) {
	pod := makePod("default", "test", corev1.PodPending,
		[]corev1.Container{{Name: "app"}},
		nil,
	)

	now := time.Now()
	ms := metricSnapshot{
		Results: []MetricResult{{
			Query: MetricQuery{Name: "Node Pressure"},
			Matrix: model.Matrix{
				{
					Metric: model.Metric{"node": "worker-1"},
					Values: []model.SamplePair{
						{Timestamp: model.TimeFromUnix(now.Unix()), Value: 1},
					},
				},
			},
		}},
	}

	findings := correlate(podSnapshot{Pod: pod}, ms)

	if len(findings) == 0 {
		t.Fatal("expected at least one finding")
	}
	if findings[0].Signal != "NODE_PRESSURE" {
		t.Errorf("expected NODE_PRESSURE, got %s", findings[0].Signal)
	}
}

func TestCorrelate_Healthy(t *testing.T) {
	pod := makePod("default", "test", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}},
		[]corev1.ContainerStatus{{
			Name:  "app",
			Ready: true,
		}},
	)

	findings := correlate(
		podSnapshot{Pod: pod},
		metricSnapshot{},
	)

	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Signal != "HEALTHY" {
		t.Errorf("expected HEALTHY, got %s", findings[0].Signal)
	}
	if findings[0].Severity != INFO {
		t.Errorf("expected INFO, got %s", findings[0].Severity)
	}
}

func TestCorrelate_SortsBySeverityDescending(t *testing.T) {
	pod := makePod("default", "test", corev1.PodRunning,
		[]corev1.Container{{Name: "app"}, {Name: "sidecar"}},
		[]corev1.ContainerStatus{
			{
				Name: "app",
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Reason:   "Error",
					},
				},
			},
			{
				Name: "sidecar",
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 137,
						Reason:   "OOMKilled",
					},
				},
			},
		},
	)

	findings := correlate(podSnapshot{Pod: pod}, metricSnapshot{})

	if len(findings) < 2 {
		t.Fatalf("expected at least 2 findings, got %d", len(findings))
	}
	// CRITICAL (OOM_KILL) should come before HIGH (APPLICATION_CRASH)
	if findings[0].Severity < findings[1].Severity {
		t.Errorf("expected findings sorted by severity descending, got %s before %s",
			findings[0].Severity, findings[1].Severity)
	}
}

// ─── parseDuration tests ─────────────────────────────────────────────────────

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"1h", false},
		{"30m", false},
		{"60s", false},
		{"1d", false},   // Prometheus-style
		{"0s", true},    // zero
		{"-5s", true},   // negative
		{"bogus", true}, // invalid
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			_, err := parseDuration(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("parseDuration(%q): err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
		})
	}
}
