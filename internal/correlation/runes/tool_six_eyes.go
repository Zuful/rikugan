package runes

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"github.com/Zuful/rikugan/pkg/provider"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const opTimeout = 60 * time.Second

// sixEyesTool returns the ToolDefinition for the six_eyes compound tool.
func (p *Provider) sixEyesTool() provider.ToolDefinition {
	tool := mcp.NewTool("six_eyes",
		mcp.WithDescription(
			"Correlate Kubernetes pod diagnostics with Prometheus metrics "+
				"to diagnose why a pod crashed or is unhealthy. "+
				"Returns a unified Markdown report with findings, pod state, events, and metrics. "+
				"[Methodology: cross-signal correlation (Step 2) — see docs/methodology.md]",
		),
		mcp.WithString("namespace",
			mcp.Required(),
			mcp.Description("Kubernetes namespace of the pod"),
		),
		mcp.WithString("pod",
			mcp.Required(),
			mcp.Description("Name of the pod to analyse"),
		),
		mcp.WithString("window",
			mcp.Description("Look-back duration for metrics, e.g. \"1h\", \"30m\" (default: \"1h\")"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleSixEyes,
	}
}

// handleSixEyes is the MCP handler for six_eyes.
func (p *Provider) handleSixEyes(
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

	window := time.Hour
	if raw, wErr := req.RequireString("window"); wErr == nil && raw != "" {
		parsed, pErr := parseDuration(raw)
		if pErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid window: %v", pErr)), nil
		}
		window = parsed
	}

	log := p.logger.With(slog.String("tool", "six_eyes"))
	log.Info("tool called",
		slog.String("namespace", ns),
		slog.String("pod", podName),
		slog.String("window", window.String()),
	)

	now := time.Now()
	start := now.Add(-window)

	// ── Phase 1: fetch pod (required for query building) ────────────
	pod, err := p.k8s.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get pod: %v", err)), nil
	}

	// ── Phase 2: fan-out events + Prometheus queries concurrently ──
	queries := buildQueries(ns, podName, pod, start, now)
	results := make([]MetricResult, len(queries))

	var events []corev1.Event

	g, gCtx := errgroup.WithContext(ctx)

	// Events — best-effort, never fails the group
	g.Go(func() error {
		eventList, eErr := p.k8s.CoreV1().Events(ns).List(gCtx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
		})
		if eErr != nil {
			log.Warn("failed to list events", slog.String("error", eErr.Error()))
			return nil
		}
		events = eventList.Items
		return nil
	})

	// Prometheus queries — each best-effort
	for i, q := range queries {
		g.Go(func() error {
			val, _, _, qErr := p.prom.QueryRange(gCtx, q.PromQL, promv1.Range{
				Start: q.Range.Start,
				End:   q.Range.End,
				Step:  q.Range.Step,
			})
			results[i] = MetricResult{Query: q, Err: qErr}
			if qErr == nil {
				results[i].Matrix = val
			}
			return nil
		})
	}
	_ = g.Wait()

	// ── Phase 3: correlate ──────────────────────────────────────────
	ps := podSnapshot{Pod: pod, Events: events}
	ms := metricSnapshot{Results: results}
	findings := correlate(ps, ms)

	// ── Build report ────────────────────────────────────────────────
	report := p.buildSixEyesReport(ns, podName, pod, events, findings, results)
	return mcp.NewToolResultText(report), nil
}

// ─── Report builder ──────────────────────────────────────────────────────────

func (p *Provider) buildSixEyesReport(
	ns, podName string,
	pod *corev1.Pod,
	events []corev1.Event,
	findings []Finding,
	results []MetricResult,
) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "## Six Eyes Correlation: %s/%s\n\n", ns, podName)

	// Diagnosis section
	writeDiagnosisSection(&b, findings)

	// Pod summary
	writePodSummary(&b, pod)

	// Events
	writeEventsSection(&b, events)

	// Metrics
	writeMetricsSection(&b, results)

	return b.String()
}

func writeDiagnosisSection(b *strings.Builder, findings []Finding) {
	fmt.Fprintf(b, "### Diagnosis\n\n")
	if len(findings) == 0 {
		fmt.Fprintf(b, "No findings.\n\n")
		return
	}

	// Top finding bold
	top := findings[0]
	fmt.Fprintf(b, "**%s** [%s]: %s\n\n", top.Signal, top.Severity, top.Explanation)

	// Additional findings as bullets
	if len(findings) > 1 {
		for _, f := range findings[1:] {
			fmt.Fprintf(b, "- **%s** [%s]: %s\n", f.Signal, f.Severity, f.Explanation)
		}
		b.WriteString("\n")
	}
}

func writePodSummary(b *strings.Builder, pod *corev1.Pod) {
	fmt.Fprintf(b, "### Pod Summary\n\n")

	phase := string(pod.Status.Phase)
	totalRestarts := int32(0)
	for _, cs := range pod.Status.ContainerStatuses {
		totalRestarts += cs.RestartCount
	}
	age := time.Since(pod.CreationTimestamp.Time).Truncate(time.Second)

	fmt.Fprintf(b, "**Phase:** %s | **Restarts:** %d | **Age:** %s\n\n", phase, totalRestarts, age)

	// Container states
	if len(pod.Status.ContainerStatuses) > 0 {
		fmt.Fprintf(b, "| Container | Ready | Restarts | State |\n")
		fmt.Fprintf(b, "|-----------|-------|----------|-------|\n")
		for _, cs := range pod.Status.ContainerStatuses {
			state := describeState(cs)
			fmt.Fprintf(b, "| %s | %v | %d | %s |\n", cs.Name, cs.Ready, cs.RestartCount, state)
		}
		b.WriteString("\n")
	}
}

func describeState(cs corev1.ContainerStatus) string {
	switch {
	case cs.State.Running != nil:
		return "Running"
	case cs.State.Waiting != nil:
		return fmt.Sprintf("Waiting: %s", cs.State.Waiting.Reason)
	case cs.State.Terminated != nil:
		return fmt.Sprintf("Terminated: exit=%d reason=%s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
	default:
		return "Unknown"
	}
}

func writeEventsSection(b *strings.Builder, events []corev1.Event) {
	fmt.Fprintf(b, "### Events\n\n")
	if len(events) == 0 {
		fmt.Fprintf(b, "No events found.\n\n")
		return
	}

	// Sort by last timestamp descending
	sort.Slice(events, func(i, j int) bool {
		return events[i].LastTimestamp.After(events[j].LastTimestamp.Time)
	})

	limit := 20
	if len(events) < limit {
		limit = len(events)
	}

	fmt.Fprintf(b, "| Type | Reason | Age | Message |\n")
	fmt.Fprintf(b, "|------|--------|-----|---------|\n")
	for _, e := range events[:limit] {
		age := time.Since(e.LastTimestamp.Time).Truncate(time.Second)
		msg := e.Message
		if len(msg) > 100 {
			msg = msg[:100] + "…"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", e.Type, e.Reason, age, msg)
	}
	b.WriteString("\n")
}

func writeMetricsSection(b *strings.Builder, results []MetricResult) {
	fmt.Fprintf(b, "### Metrics\n\n")

	for _, r := range results {
		fmt.Fprintf(b, "#### %s\n\n", r.Query.Name)
		fmt.Fprintf(b, "%s\n\n", r.Query.Description)

		if r.Err != nil {
			fmt.Fprintf(b, "_Error: %v_\n\n", r.Err)
			continue
		}

		matrix, ok := r.Matrix.(model.Matrix)
		if !ok || len(matrix) == 0 {
			fmt.Fprintf(b, "_No data._\n\n")
			continue
		}

		for _, series := range matrix {
			fmt.Fprintf(b, "**Labels:** `%s`\n\n", series.Metric.String())
			if len(series.Values) == 0 {
				fmt.Fprintf(b, "_No data points._\n\n")
				continue
			}
			fmt.Fprintf(b, "| Timestamp | Value |\n")
			fmt.Fprintf(b, "|-----------|-------|\n")
			for _, sp := range series.Values {
				fmt.Fprintf(b, "| %s | %s |\n",
					sp.Timestamp.Time().UTC().Format(time.RFC3339),
					sp.Value.String(),
				)
			}
			b.WriteString("\n")
		}
	}
}

// ─── Duration helper ─────────────────────────────────────────────────────────

// parseDuration tries Go's time.ParseDuration first, then falls back to
// Prometheus model.ParseDuration (which supports "1d", "1w" etc.).
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("duration must be positive, got %s", s)
		}
		return d, nil
	}
	pd, promErr := model.ParseDuration(s)
	if promErr != nil {
		return 0, fmt.Errorf("cannot parse duration %q: %w", s, promErr)
	}
	d = time.Duration(pd)
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %s", s)
	}
	return d, nil
}
