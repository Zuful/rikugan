package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultTailLines = 50
	maxTailLines     = 1000
	maxEvents        = 20
	opTimeout        = 30 * time.Second
)

// diagnosePodTool returns the ToolDefinition for the diagnose_pod compound tool.
func (p *Provider) diagnosePodTool() provider.ToolDefinition {
	tool := mcp.NewTool("diagnose_pod",
		mcp.WithDescription(
			"Diagnose a Kubernetes pod by gathering its status, conditions, "+
				"recent events, and container logs in a single call. "+
				"Returns a structured Markdown report. "+
				"[Methodology: Errors and Saturation signals of LEST (pod crashes, OOM, resource limits) — see docs/methodology.md]",
		),
		mcp.WithString("namespace",
			mcp.Required(),
			mcp.Description("Kubernetes namespace of the pod"),
		),
		mcp.WithString("pod",
			mcp.Required(),
			mcp.Description("Name of the pod to diagnose"),
		),
		mcp.WithNumber("tail",
			mcp.Description("Number of log lines to retrieve per container (default 50)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleDiagnosePod,
	}
}

// handleDiagnosePod is the MCP handler for diagnose_pod.
func (p *Provider) handleDiagnosePod(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	// ── Timeout ─────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	// ── Parse arguments ─────────────────────────────────────────────
	ns, err := req.RequireString("namespace")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	podName, err := req.RequireString("pod")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	tailInt := req.GetInt("tail", defaultTailLines)
	if tailInt <= 0 {
		tailInt = defaultTailLines
	}
	if tailInt > maxTailLines {
		tailInt = maxTailLines
	}
	tail := int64(tailInt)

	log := p.logger.With(slog.String("tool", "diagnose_pod"))
	log.Info("tool called",
		slog.String("namespace", ns),
		slog.String("pod", podName),
		slog.Int64("tail", tail),
	)

	// ── Phase 1: fetch pod (needed by both logs and the report) ─────
	pod, err := p.client.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get pod: %v", err)), nil
	}

	// ── Phase 2: fan-out events + logs concurrently ─────────────────
	var (
		events []corev1.Event
		logs   map[string]string // container name → log text
	)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		eventList, gErr := p.client.CoreV1().Events(ns).List(gCtx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
		})
		if gErr != nil {
			log.Warn("failed to list events", slog.String("error", gErr.Error()))
			return nil // best-effort — degrade gracefully
		}
		events = eventList.Items
		return nil
	})

	g.Go(func() error {
		logs = p.fetchLogs(gCtx, pod, tail)
		return nil // log fetch is best-effort; errors are inlined
	})

	_ = g.Wait() // all goroutines are best-effort, no error possible

	// ── Build report ────────────────────────────────────────────────
	report := p.buildReport(ns, podName, pod, events, logs)
	return mcp.NewToolResultText(report), nil
}

// fetchLogs retrieves the last `tail` lines from every container in the pod
// concurrently. If a container has restarted, it also fetches logs from the
// previous instance. Errors are captured inline (best-effort).
func (p *Provider) fetchLogs(
	ctx context.Context,
	pod *corev1.Pod,
	tail int64,
) map[string]string {
	ns := pod.Namespace
	podName := pod.Name

	// Build a restart lookup for O(1) access.
	restarts := make(map[string]int32, len(pod.Status.ContainerStatuses))
	for _, cs := range pod.Status.ContainerStatuses {
		restarts[cs.Name] = cs.RestartCount
	}

	// Pre-allocate result slots so goroutines write to disjoint keys.
	result := make(map[string]string, len(pod.Spec.Containers)*2)
	var mu sync.Mutex

	g, gCtx := errgroup.WithContext(ctx)
	for _, c := range pod.Spec.Containers {
		name := c.Name

		// Current logs
		g.Go(func() error {
			text := p.readLog(gCtx, ns, podName, name, tail, false)
			mu.Lock()
			result[name] = text
			mu.Unlock()
			return nil
		})

		// Previous-instance logs if container has restarted
		if restarts[name] > 0 {
			g.Go(func() error {
				text := p.readLog(gCtx, ns, podName, name, tail, true)
				if text != "" {
					mu.Lock()
					result[name+" (previous)"] = text
					mu.Unlock()
				}
				return nil
			})
		}
	}

	_ = g.Wait()
	return result
}

// readLog fetches log lines for a single container.
func (p *Provider) readLog(
	ctx context.Context,
	ns, podName, container string,
	tail int64,
	previous bool,
) string {
	opts := &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
		Previous:  previous,
	}

	stream, err := p.client.CoreV1().Pods(ns).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return fmt.Sprintf("[error fetching logs: %v]", err)
	}
	defer func() { _ = stream.Close() }()

	var buf bytes.Buffer
	if _, err = io.Copy(&buf, stream); err != nil {
		return fmt.Sprintf("[error reading log stream: %v]", err)
	}

	return buf.String()
}

// ─── Report builder ─────────────────────────────────────────────────────────

func (p *Provider) buildReport(
	ns, podName string,
	pod *corev1.Pod,
	events []corev1.Event,
	logs map[string]string,
) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "## Pod Diagnostic: %s/%s\n\n", ns, podName)

	// Status summary
	p.writeStatusSection(&b, pod)

	// Conditions table
	p.writeConditionsSection(&b, pod)

	// Container statuses
	p.writeContainerStatusSection(&b, pod)

	// Events
	p.writeEventsSection(&b, events)

	// Logs
	p.writeLogsSection(&b, logs)

	return b.String()
}

func (p *Provider) writeStatusSection(b *strings.Builder, pod *corev1.Pod) {
	phase := string(pod.Status.Phase)
	statusEmoji := "✅"

	// Detect problematic states
	var warnings []string
	totalRestarts := int32(0)
	for _, cs := range pod.Status.ContainerStatuses {
		totalRestarts += cs.RestartCount
		if cs.State.Waiting != nil {
			if cs.State.Waiting.Reason == "CrashLoopBackOff" {
				warnings = append(warnings, "CrashLoopBackOff")
				statusEmoji = "🔴"
			} else if cs.State.Waiting.Reason != "" {
				warnings = append(warnings, cs.State.Waiting.Reason)
				statusEmoji = "⚠️"
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			warnings = append(warnings, fmt.Sprintf(
				"container %q exited with code %d",
				cs.Name, cs.State.Terminated.ExitCode,
			))
			statusEmoji = "⚠️"
		}
	}

	headline := phase
	if len(warnings) > 0 {
		headline = strings.Join(warnings, ", ")
	}

	age := time.Since(pod.CreationTimestamp.Time).Truncate(time.Second)

	fmt.Fprintf(b, "### Status: %s %s\n", headline, statusEmoji)
	fmt.Fprintf(b, "**Phase:** %s | **Restarts:** %d | **Age:** %s\n\n",
		phase, totalRestarts, age)
}

func (p *Provider) writeConditionsSection(b *strings.Builder, pod *corev1.Pod) {
	if len(pod.Status.Conditions) == 0 {
		return
	}
	fmt.Fprintf(b, "### Conditions\n\n")
	fmt.Fprintf(b, "| Type | Status | Reason | Message |\n")
	fmt.Fprintf(b, "|------|--------|--------|---------|\n")
	for _, c := range pod.Status.Conditions {
		msg := c.Message
		if len(msg) > 80 {
			msg = msg[:80] + "…"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			c.Type, c.Status, c.Reason, msg)
	}
	b.WriteString("\n")
}

func (p *Provider) writeContainerStatusSection(b *strings.Builder, pod *corev1.Pod) {
	if len(pod.Status.ContainerStatuses) == 0 {
		return
	}
	fmt.Fprintf(b, "### Container Statuses\n\n")
	fmt.Fprintf(b, "| Container | Ready | Restarts | State | Detail |\n")
	fmt.Fprintf(b, "|-----------|-------|----------|-------|--------|\n")
	for _, cs := range pod.Status.ContainerStatuses {
		state, detail := describeContainerState(cs)
		fmt.Fprintf(b, "| %s | %v | %d | %s | %s |\n",
			cs.Name, cs.Ready, cs.RestartCount, state, detail)
	}
	b.WriteString("\n")
}

func describeContainerState(cs corev1.ContainerStatus) (string, string) {
	switch {
	case cs.State.Running != nil:
		return "Running", fmt.Sprintf("since %s", cs.State.Running.StartedAt.Format(time.RFC3339))
	case cs.State.Waiting != nil:
		return "Waiting", cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
	case cs.State.Terminated != nil:
		t := cs.State.Terminated
		return "Terminated", fmt.Sprintf("exit=%d reason=%s", t.ExitCode, t.Reason)
	default:
		return "Unknown", ""
	}
}

func (p *Provider) writeEventsSection(b *strings.Builder, events []corev1.Event) {
	if len(events) == 0 {
		fmt.Fprintf(b, "### Events\n\nNo events found.\n\n")
		return
	}

	// Sort by last timestamp descending
	sort.Slice(events, func(i, j int) bool {
		ti := events[i].LastTimestamp.Time
		tj := events[j].LastTimestamp.Time
		return ti.After(tj)
	})

	limit := maxEvents
	if len(events) < limit {
		limit = len(events)
	}

	fmt.Fprintf(b, "### Events (latest %d)\n\n", limit)
	fmt.Fprintf(b, "| Type | Reason | Age | Message |\n")
	fmt.Fprintf(b, "|------|--------|-----|---------|\n")
	for _, e := range events[:limit] {
		age := time.Since(e.LastTimestamp.Time).Truncate(time.Second)
		msg := e.Message
		if len(msg) > 100 {
			msg = msg[:100] + "…"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			e.Type, e.Reason, age, msg)
	}
	b.WriteString("\n")
}

func (p *Provider) writeLogsSection(b *strings.Builder, logs map[string]string) {
	if len(logs) == 0 {
		return
	}

	fmt.Fprintf(b, "### Logs\n\n")
	for name, text := range logs {
		fmt.Fprintf(b, "#### Container: %s\n\n", name)
		fmt.Fprintf(b, "```\n%s```\n\n", text)
	}
}
