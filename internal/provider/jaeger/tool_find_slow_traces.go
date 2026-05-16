package jaeger

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const opTimeout = 30 * time.Second

// findSlowTracesTool returns the ToolDefinition for find_slow_traces.
func (p *Provider) findSlowTracesTool() provider.ToolDefinition {
	tool := mcp.NewTool("find_slow_traces",
		mcp.WithDescription(
			"Find the most recent slow traces for a service from Jaeger. "+
				"Returns a summary table with trace ID, start time, and total duration. "+
				"Useful for identifying latency issues in microservice architectures. "+
				"[Methodology: Latency signal of LEST — see docs/methodology.md]",
		),
		mcp.WithString("service",
			mcp.Required(),
			mcp.Description("Service name to search traces for"),
		),
		mcp.WithString("min_duration",
			mcp.Description("Minimum trace duration to filter by (e.g. \"500ms\", \"1s\", \"2.5s\"). Default: no minimum"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleFindSlowTraces,
	}
}

// handleFindSlowTraces is the MCP handler for find_slow_traces.
func (p *Provider) handleFindSlowTraces(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	service, err := req.RequireString("service")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	minDuration, _ := req.RequireString("min_duration")

	log := p.logger.With(slog.String("tool", "find_slow_traces"))
	log.Info("tool called",
		"service", service,
		"min_duration", minDuration,
	)

	traces, err := p.client.FindTraces(ctx, service, 5, minDuration)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("jaeger API error: %v", err)), nil
	}

	report := buildSlowTracesReport(service, minDuration, traces)
	return mcp.NewToolResultText(report), nil
}

func buildSlowTracesReport(service, minDuration string, traces []Trace) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Slow Traces\n\n")
	fmt.Fprintf(&b, "**Service:** `%s`\n", service)
	if minDuration != "" {
		fmt.Fprintf(&b, "**Minimum duration:** %s\n", minDuration)
	}
	b.WriteString("\n")

	if len(traces) == 0 {
		b.WriteString("_No traces found matching the criteria._\n")
		return b.String()
	}

	b.WriteString("| Trace ID | Start Time | Duration | Spans |\n")
	b.WriteString("|----------|------------|----------|-------|\n")

	for _, t := range traces {
		if len(t.Spans) == 0 {
			continue
		}

		// Compute trace duration and start time from spans.
		minStart := t.Spans[0].StartTime
		var maxEnd int64
		for _, s := range t.Spans {
			if s.StartTime < minStart {
				minStart = s.StartTime
			}
			end := s.StartTime + s.Duration
			if end > maxEnd {
				maxEnd = end
			}
		}

		startTime := time.UnixMicro(minStart).UTC().Format(time.RFC3339)
		duration := time.Duration(maxEnd-minStart) * time.Microsecond

		fmt.Fprintf(&b, "| `%s` | %s | %s | %d |\n",
			t.TraceID, startTime, formatDuration(duration), len(t.Spans),
		)
	}

	return b.String()
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
