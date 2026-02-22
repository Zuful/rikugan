package prometheus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/common/model"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"github.com/Zuful/rikugan/internal/provider"
)

const (
	opTimeout     = 30 * time.Second
	maxDataPoints = 10_000
	defaultStep   = "60s"
	defaultRange  = 1 * time.Hour
)

// queryRangeTool returns the ToolDefinition for the query_range tool.
func (p *PrometheusProvider) queryRangeTool() provider.ToolDefinition {
	tool := mcp.NewTool("query_range",
		mcp.WithDescription(
			"Execute a PromQL range query against Prometheus and return a "+
				"structured Markdown report with time-series data. "+
				"Useful for analysing metrics over a time window.",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("PromQL expression to evaluate"),
		),
		mcp.WithString("start",
			mcp.Description("Range start time in RFC3339 format (default: 1 hour ago)"),
		),
		mcp.WithString("end",
			mcp.Description("Range end time in RFC3339 format (default: now)"),
		),
		mcp.WithString("step",
			mcp.Description("Query resolution step as a duration string, e.g. \"60s\", \"5m\" (default: \"60s\")"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleQueryRange,
	}
}

// handleQueryRange is the MCP handler for query_range.
func (p *PrometheusProvider) handleQueryRange(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	// ── Timeout ─────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	// ── Parse arguments ─────────────────────────────────────────────
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	now := time.Now()

	start, err := parseTimeArg(req, "start", now.Add(-defaultRange))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid start: %v", err)), nil
	}

	end, err := parseTimeArg(req, "end", now)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid end: %v", err)), nil
	}

	if !end.After(start) {
		return mcp.NewToolResultError("end must be after start"), nil
	}

	step, err := parseStepArg(req)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid step: %v", err)), nil
	}

	// ── Guard against excessive data ────────────────────────────────
	points := int(end.Sub(start)/step) + 1
	if points > maxDataPoints {
		return mcp.NewToolResultError(fmt.Sprintf(
			"query would return ~%d data points (max %d); narrow the range or increase step",
			points, maxDataPoints,
		)), nil
	}

	log := p.logger.With(slog.String("tool", "query_range"))
	log.Info("tool called",
		"query", query,
		"start", start.Format(time.RFC3339),
		"end", end.Format(time.RFC3339),
		"step", step.String(),
	)

	// ── Execute query ───────────────────────────────────────────────
	result, warnings, err := p.api.QueryRange(ctx, query, promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("prometheus query failed: %v", err)), nil
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf(
			"unexpected result type %T; expected matrix", result,
		)), nil
	}

	// ── Build report ────────────────────────────────────────────────
	report := buildQueryRangeReport(query, start, end, step, matrix, warnings)
	return mcp.NewToolResultText(report), nil
}

// ─── Argument helpers ────────────────────────────────────────────────────────

func parseTimeArg(req mcp.CallToolRequest, name string, fallback time.Time) (time.Time, error) {
	raw, err := req.RequireString(name)
	if err != nil {
		// Not provided — use fallback.
		return fallback, nil
	}
	t, parseErr := time.Parse(time.RFC3339, raw)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q as RFC3339: %w", raw, parseErr)
	}
	return t, nil
}

func parseStepArg(req mcp.CallToolRequest) (time.Duration, error) {
	raw, err := req.RequireString("step")
	if err != nil {
		// Not provided — use default.
		return parseDuration(defaultStep)
	}
	return parseDuration(raw)
}

// parseDuration tries Go's time.ParseDuration first, then falls back to
// Prometheus model.ParseDuration (which supports "1d", "1w" etc.).
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("step must be positive, got %s", s)
		}
		return d, nil
	}
	pd, promErr := model.ParseDuration(s)
	if promErr != nil {
		return 0, fmt.Errorf("cannot parse duration %q: %w", s, promErr)
	}
	d = time.Duration(pd)
	if d <= 0 {
		return 0, fmt.Errorf("step must be positive, got %s", s)
	}
	return d, nil
}

// ─── Report builder ──────────────────────────────────────────────────────────

func buildQueryRangeReport(
	query string,
	start, end time.Time,
	step time.Duration,
	matrix model.Matrix,
	warnings promv1.Warnings,
) string {
	var b strings.Builder

	writeQueryInfoSection(&b, query, start, end, step)
	writeWarningsSection(&b, warnings)

	fmt.Fprintf(&b, "**Series returned:** %d\n\n", len(matrix))

	for i, series := range matrix {
		writeSeriesSection(&b, i+1, series)
	}

	return b.String()
}

func writeQueryInfoSection(b *strings.Builder, query string, start, end time.Time, step time.Duration) {
	fmt.Fprintf(b, "## PromQL Range Query Results\n\n")
	fmt.Fprintf(b, "```promql\n%s\n```\n\n", query)
	fmt.Fprintf(b, "| Parameter | Value |\n")
	fmt.Fprintf(b, "|-----------|-------|\n")
	fmt.Fprintf(b, "| Start | %s |\n", start.Format(time.RFC3339))
	fmt.Fprintf(b, "| End | %s |\n", end.Format(time.RFC3339))
	fmt.Fprintf(b, "| Step | %s |\n", step)
	fmt.Fprintf(b, "\n")
}

func writeWarningsSection(b *strings.Builder, warnings promv1.Warnings) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintf(b, "### Warnings\n\n")
	for _, w := range warnings {
		fmt.Fprintf(b, "- %s\n", w)
	}
	b.WriteString("\n")
}

func writeSeriesSection(b *strings.Builder, idx int, series *model.SampleStream) {
	fmt.Fprintf(b, "### Series %d\n\n", idx)

	// Labels
	fmt.Fprintf(b, "**Labels:** `%s`\n\n", series.Metric.String())

	if len(series.Values) == 0 {
		fmt.Fprintf(b, "_No data points._\n\n")
		return
	}

	// Timestamp / Value table
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
