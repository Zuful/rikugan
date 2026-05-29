package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	opTimeout    = 30 * time.Second
	defaultRange = 1 * time.Hour
	defaultStep  = 60 * time.Second
)

// Compile-time check that *ConceptProvider satisfies provider.Provider
// so the dispatcher can register it like any other provider.
var _ provider.Provider = (*ConceptProvider)(nil)

// ConceptProvider is the concept-level tool registry for the metrics
// concept. It wraps the operator-configured provider.MetricsProvider
// implementation (prometheus today, kubectl-top tomorrow) and exposes
// three technology-agnostic tools — get_metric, compare_to_baseline,
// list_available_metrics — that route through the interface and
// format the result as Markdown for MCP clients.
type ConceptProvider struct {
	impl     provider.MetricsProvider
	implName string
	logger   *slog.Logger
}

// NewConceptProvider wires the concept tools to a specific
// MetricsProvider implementation. implName is the backend's short
// label (e.g. "prometheus") surfaced in report headers and logs.
func NewConceptProvider(impl provider.MetricsProvider, implName string, logger *slog.Logger) *ConceptProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConceptProvider{
		impl:     impl,
		implName: implName,
		logger:   logger.With(slog.String("provider", "metrics"), slog.String("backend", implName)),
	}
}

// Name returns the provider identifier surfaced to the dispatcher.
func (c *ConceptProvider) Name() string { return "metrics" }

// Tools returns the canonical Metrics concept tools.
func (c *ConceptProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		c.getMetricTool(),
		c.compareToBaselineTool(),
		c.listAvailableMetricsTool(),
	}
}

// ── Tool definitions ────────────────────────────────────────────────────────

func (c *ConceptProvider) getMetricTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_metric",
		mcp.WithDescription(
			"Evaluate a query against the configured metrics backend "+
				"(prometheus today) over a time window and return a structured "+
				"Markdown report with series and data points. "+
				"[Methodology: any LEST signal depending on the query — Latency "+
				"(histogram_quantile on duration), Errors (5xx rate), Saturation "+
				"(CPU/memory), Traffic (request rate) — see docs/methodology.md]",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Backend-native query expression (PromQL for prometheus)."),
		),
		mcp.WithString("start", mcp.Description("Start time in RFC3339 (default: 1 hour ago).")),
		mcp.WithString("end", mcp.Description("End time in RFC3339 (default: now).")),
		mcp.WithString("step", mcp.Description("Resolution step as a duration string, e.g. \"60s\" (default: 60s).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleGetMetric}
}

func (c *ConceptProvider) compareToBaselineTool() provider.ToolDefinition {
	tool := mcp.NewTool("compare_to_baseline",
		mcp.WithDescription(
			"Run the same query for the most recent `window` and for a "+
				"baseline window ending `baseline` ago, then report the mean "+
				"of each side plus the delta. Useful for spotting regressions. "+
				"[Methodology: any LEST signal — comparison detects deviation "+
				"from the historical baseline — see docs/methodology.md]",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Backend-native query expression (PromQL for prometheus)."),
		),
		mcp.WithString("window", mcp.Description("Width of both the current and baseline windows (e.g. \"1h\", \"15m\"). Default 15m.")),
		mcp.WithString("baseline", mcp.Description("How far back the baseline window ends, relative to now (e.g. \"24h\", \"7d\"). Default 24h.")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleCompareToBaseline}
}

func (c *ConceptProvider) listAvailableMetricsTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_available_metrics",
		mcp.WithDescription(
			"List metric names known to the configured backend, optionally "+
				"filtered by a prefix. Useful for discovering what to query.",
		),
		mcp.WithString("prefix", mcp.Description("Optional metric-name prefix to filter on (case-sensitive). Empty returns everything.")),
		mcp.WithNumber("limit", mcp.Description("Max names to return (default 200, hard cap 2000).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleListAvailableMetrics}
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *ConceptProvider) handleGetMetric(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	now := time.Now()
	start := now.Add(-defaultRange)
	end := now
	if v := req.GetString("start", ""); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			start = t
		}
	}
	if v := req.GetString("end", ""); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			end = t
		}
	}
	step := defaultStep
	if v := req.GetString("step", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			step = d
		}
	}

	samples, err := c.impl.GetMetric(ctx, query, start, end, step)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("metrics backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildGetMetricReport(c.implName, query, start, end, step, samples)), nil
}

func (c *ConceptProvider) handleCompareToBaseline(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	window := 15 * time.Minute
	if v := req.GetString("window", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			window = d
		}
	}
	baseline := 24 * time.Hour
	if v := req.GetString("baseline", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			baseline = d
		}
	}

	report, err := c.impl.CompareToBaseline(ctx, query, window, baseline)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("metrics backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(report), nil
}

func (c *ConceptProvider) handleListAvailableMetrics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	prefix := req.GetString("prefix", "")
	limit := 200
	if v, err := req.RequireFloat("limit"); err == nil && v > 0 {
		limit = int(v)
	}
	if limit > 2000 {
		limit = 2000
	}

	names, err := c.impl.ListAvailableMetrics(ctx, prefix)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("metrics backend %q error: %v", c.implName, err)), nil
	}
	if len(names) > limit {
		names = names[:limit]
	}
	return mcp.NewToolResultText(buildListMetricsReport(c.implName, prefix, names)), nil
}

// ── Report builders ─────────────────────────────────────────────────────────

func buildGetMetricReport(
	backend, query string,
	start, end time.Time,
	step time.Duration,
	samples []provider.MetricSample,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Metric Query Results\n\n")
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	fmt.Fprintf(&b, "```\n%s\n```\n\n", query)
	fmt.Fprintf(&b, "| Parameter | Value |\n|-----------|-------|\n")
	fmt.Fprintf(&b, "| Start | %s |\n", start.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| End | %s |\n", end.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Step | %s |\n", step)
	fmt.Fprintf(&b, "| Samples | %d |\n\n", len(samples))

	if len(samples) == 0 {
		b.WriteString("_No samples returned._\n")
		return b.String()
	}

	// Group samples by their label fingerprint so the report renders one
	// section per series — same layout as the legacy query_range tool.
	type series struct {
		labels  string
		samples []provider.MetricSample
	}
	groups := map[string]*series{}
	var order []string
	for _, s := range samples {
		key := fingerprintLabels(s.Labels)
		g, ok := groups[key]
		if !ok {
			g = &series{labels: key}
			groups[key] = g
			order = append(order, key)
		}
		g.samples = append(g.samples, s)
	}
	sort.Strings(order)
	fmt.Fprintf(&b, "**Series returned:** %d\n\n", len(order))
	for i, key := range order {
		g := groups[key]
		fmt.Fprintf(&b, "### Series %d\n\n", i+1)
		fmt.Fprintf(&b, "**Labels:** `%s`\n\n", g.labels)
		fmt.Fprintf(&b, "| Timestamp | Value |\n|-----------|-------|\n")
		for _, s := range g.samples {
			fmt.Fprintf(&b, "| %s | %g |\n", s.Time.UTC().Format(time.RFC3339), s.Value)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func buildListMetricsReport(backend, prefix string, names []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Available Metrics\n\n")
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	if prefix != "" {
		fmt.Fprintf(&b, "**Prefix:** `%s`  \n", prefix)
	}
	fmt.Fprintf(&b, "**Returned:** %d\n\n", len(names))
	if len(names) == 0 {
		b.WriteString("_No metric names match._\n")
		return b.String()
	}
	for _, n := range names {
		fmt.Fprintf(&b, "- `%s`\n", n)
	}
	return b.String()
}

func fingerprintLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
