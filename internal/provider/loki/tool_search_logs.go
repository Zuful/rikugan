package loki

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const (
	opTimeout     = 30 * time.Second
	defaultLimit  = 100
	maxLimit      = 500
	maxLogLineLen = 200
)

// buildLabelQuery constructs a LogQL stream selector from service and namespace.
func buildLabelQuery(service, namespace string) string {
	var parts []string
	if service != "" {
		parts = append(parts, fmt.Sprintf(`app="%s"`, service))
	}
	if namespace != "" {
		parts = append(parts, fmt.Sprintf(`namespace="%s"`, namespace))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// logEntry is a flattened log line with its metadata.
type logEntry struct {
	Timestamp time.Time
	Labels    string
	Line      string
}

// searchLogsTool returns the ToolDefinition for search_logs.
func (p *Provider) searchLogsTool() provider.ToolDefinition {
	tool := mcp.NewTool("search_logs",
		mcp.WithDescription(
			"Search logs in Loki using a LogQL expression. "+
				"Returns matching log lines with timestamps and stream labels. "+
				"Useful for finding specific error messages, patterns, or events across services. "+
				"[Methodology: Errors signal of LEST (and Traffic when querying access-log volume) — see docs/methodology.md]",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("LogQL expression (e.g. {app=\"payment-service\"} |= \"error\")"),
		),
		mcp.WithString("start",
			mcp.Description("Start time in RFC3339 format (default: 1 hour ago)"),
		),
		mcp.WithString("end",
			mcp.Description("End time in RFC3339 format (default: now)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of log lines to return (default: 100, max: 500)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleSearchLogs,
	}
}

// handleSearchLogs is the MCP handler for search_logs.
func (p *Provider) handleSearchLogs(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	startStr, _ := req.RequireString("start")
	endStr, _ := req.RequireString("end")

	limit := defaultLimit
	if v, err := req.RequireFloat("limit"); err == nil {
		limit = int(v)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	now := time.Now()
	startTime := now.Add(-1 * time.Hour)
	endTime := now

	if startStr != "" {
		if t, err := time.Parse(time.RFC3339, startStr); err == nil {
			startTime = t
		}
	}
	if endStr != "" {
		if t, err := time.Parse(time.RFC3339, endStr); err == nil {
			endTime = t
		}
	}

	log := p.logger.With(slog.String("tool", "search_logs"))
	log.Info("tool called",
		"query", query,
		"start", startTime.Format(time.RFC3339),
		"end", endTime.Format(time.RFC3339),
		"limit", limit,
	)

	streams, err := p.client.QueryRange(ctx, query, ToNano(startTime), ToNano(endTime), limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loki API error: %v", err)), nil
	}

	entries := flattenStreams(streams)

	report := buildSearchLogsReport(query, startTime, endTime, limit, entries)
	return mcp.NewToolResultText(report), nil
}

// flattenStreams converts Loki streams into sorted log entries.
func flattenStreams(streams []stream) []logEntry {
	var entries []logEntry
	for _, s := range streams {
		labels := formatLabels(s.Stream)
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			ts := parseNanoTimestamp(v[0])
			line := v[1]
			if len(line) > maxLogLineLen {
				line = line[:maxLogLineLen-3] + "..."
			}
			entries = append(entries, logEntry{
				Timestamp: ts,
				Labels:    labels,
				Line:      line,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.After(entries[j].Timestamp)
	})

	return entries
}

// formatLabels renders a label map as a compact string.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	var parts []string
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// parseNanoTimestamp parses a nanosecond-epoch string into time.Time.
func parseNanoTimestamp(ns string) time.Time {
	var nanos int64
	fmt.Sscanf(ns, "%d", &nanos)
	return time.Unix(0, nanos)
}

func buildSearchLogsReport(query string, start, end time.Time, limit int, entries []logEntry) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Log Search Results\n\n")
	fmt.Fprintf(&b, "**Query:** `%s`  \n", query)
	fmt.Fprintf(&b, "**Range:** %s → %s  \n", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "**Lines returned:** %d (limit: %d)\n\n", len(entries), limit)

	if len(entries) == 0 {
		b.WriteString("_No log lines found matching the query._\n")
		return b.String()
	}

	b.WriteString("| Timestamp | Stream | Log Line |\n")
	b.WriteString("|-----------|--------|----------|\n")

	for _, e := range entries {
		fmt.Fprintf(&b, "| %s | %s | %s |\n",
			e.Timestamp.UTC().Format(time.RFC3339),
			e.Labels,
			e.Line,
		)
	}

	return b.String()
}
