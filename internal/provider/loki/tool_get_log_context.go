package loki

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const defaultWindow = 5 * time.Minute

// getLogContextTool returns the ToolDefinition for get_log_context.
func (p *LokiProvider) getLogContextTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_log_context",
		mcp.WithDescription(
			"Retrieve logs surrounding a specific timestamp for a service. "+
				"This is a bridge tool — use it when you find a timestamp from a trace "+
				"or event and want to see the surrounding log lines for context.",
		),
		mcp.WithString("service",
			mcp.Required(),
			mcp.Description("Service name (used as the app label in LogQL)"),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace to scope the query"),
		),
		mcp.WithString("timestamp",
			mcp.Required(),
			mcp.Description("Center timestamp in RFC3339 format"),
		),
		mcp.WithString("window",
			mcp.Description("Time window around the timestamp (e.g. \"5m\", \"10m\"). Default: 5m"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetLogContext,
	}
}

// handleGetLogContext is the MCP handler for get_log_context.
func (p *LokiProvider) handleGetLogContext(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	service, err := req.RequireString("service")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	namespace, _ := req.RequireString("namespace")

	tsStr, err := req.RequireString("timestamp")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	center, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid timestamp: %v", err)), nil
	}

	window := defaultWindow
	if w, _ := req.RequireString("window"); w != "" {
		if d, err := time.ParseDuration(w); err == nil && d > 0 {
			window = d
		}
	}

	log := p.logger.With(slog.String("tool", "get_log_context"))
	log.Info("tool called",
		"service", service,
		"namespace", namespace,
		"timestamp", tsStr,
		"window", window.String(),
	)

	query := buildLabelQuery(service, namespace)
	startTime := center.Add(-window)
	endTime := center.Add(window)

	streams, err := p.client.QueryRange(ctx, query, ToNano(startTime), ToNano(endTime), maxLimit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loki API error: %v", err)), nil
	}

	entries := flattenStreams(streams)

	report := buildLogContextReport(service, namespace, center, window, entries)
	return mcp.NewToolResultText(report), nil
}

func buildLogContextReport(service, namespace string, center time.Time, window time.Duration, entries []logEntry) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Log Context\n\n")
	fmt.Fprintf(&b, "**Service:** `%s`  \n", service)
	if namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`  \n", namespace)
	}
	fmt.Fprintf(&b, "**Center:** %s  \n", center.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "**Window:** ±%s  \n", window.String())
	fmt.Fprintf(&b, "**Lines returned:** %d\n\n", len(entries))

	if len(entries) == 0 {
		b.WriteString("_No log lines found in the specified window._\n")
		return b.String()
	}

	b.WriteString("| Timestamp | Log Line |\n")
	b.WriteString("|-----------|----------|\n")

	for _, e := range entries {
		fmt.Fprintf(&b, "| %s | %s |\n",
			e.Timestamp.UTC().Format(time.RFC3339),
			e.Line,
		)
	}

	return b.String()
}
