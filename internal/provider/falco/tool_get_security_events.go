package falco

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const (
	opTimeout      = 30 * time.Second
	defaultLookback = 24 * time.Hour
	maxEvents       = 50
)

// getSecurityEventsTool returns the ToolDefinition for get_security_events.
func (p *Provider) getSecurityEventsTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_security_events",
		mcp.WithDescription(
			"Fetch recent Falco security events from Falcosidekick. "+
				"Can filter by namespace, pod name, and minimum severity. "+
				"Returns a Markdown table of events with rule, severity, time, and output. "+
				"[Methodology: Errors signal of LEST (runtime anomalies) — see docs/methodology.md]",
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace to filter events by"),
		),
		mcp.WithString("pod_name",
			mcp.Description("Pod name to filter events by"),
		),
		mcp.WithString("severity",
			mcp.Description("Minimum severity level: Notice, Warning, Error, Critical (default: Warning)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetSecurityEvents,
	}
}

// handleGetSecurityEvents is the MCP handler for get_security_events.
func (p *Provider) handleGetSecurityEvents(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	namespace, _ := req.RequireString("namespace")
	podName, _ := req.RequireString("pod_name")
	severity, _ := req.RequireString("severity")
	if severity == "" {
		severity = "Warning"
	}

	log := p.logger.With(slog.String("tool", "get_security_events"))
	log.Info("tool called",
		"namespace", namespace,
		"pod_name", podName,
		"severity", severity,
	)

	events, err := p.client.ListEvents(ctx, severity, defaultLookback)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("falcosidekick API error: %v", err)), nil
	}

	// Client-side filtering by namespace and pod.
	filtered := filterEvents(events, namespace, podName)

	// Cap output to protect context window.
	if len(filtered) > maxEvents {
		filtered = filtered[:maxEvents]
	}

	report := buildSecurityEventsReport(namespace, podName, severity, filtered, len(events))
	return mcp.NewToolResultText(report), nil
}

func filterEvents(events []Event, namespace, podName string) []Event {
	if namespace == "" && podName == "" {
		return events
	}
	var out []Event
	for _, e := range events {
		if namespace != "" && e.FieldString("k8s.ns.name") != namespace {
			continue
		}
		if podName != "" && e.FieldString("k8s.pod.name") != podName {
			continue
		}
		out = append(out, e)
	}
	return out
}

func buildSecurityEventsReport(namespace, podName, severity string, events []Event, totalFetched int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Falco Security Events\n\n")
	if namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`  \n", namespace)
	}
	if podName != "" {
		fmt.Fprintf(&b, "**Pod:** `%s`  \n", podName)
	}
	fmt.Fprintf(&b, "**Minimum severity:** %s  \n", severity)
	fmt.Fprintf(&b, "**Events shown:** %d (of %d fetched)\n\n", len(events), totalFetched)

	if len(events) == 0 {
		b.WriteString("_No security events found matching the criteria._\n")
		return b.String()
	}

	b.WriteString("| Time | Severity | Rule | Output |\n")
	b.WriteString("|------|----------|------|--------|\n")

	for _, e := range events {
		output := e.Output
		if len(output) > 100 {
			output = output[:97] + "..."
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			e.Time.UTC().Format(time.RFC3339),
			e.Priority,
			e.Rule,
			output,
		)
	}

	return b.String()
}
