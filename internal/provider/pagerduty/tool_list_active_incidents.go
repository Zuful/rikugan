package pagerduty

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const (
	activeIncidentsTimeout = 30 * time.Second
	activeIncidentsLimit   = 50
)

// listActiveIncidentsTool returns the ToolDefinition for list_active_incidents.
func (p *Provider) listActiveIncidentsTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_active_incidents",
		mcp.WithDescription(
			"List PagerDuty incidents that are currently triggered or acknowledged "+
				"(i.e. not yet resolved). Optionally filter by service name. "+
				"Call this BEFORE running the LEST playbook — if an incident is "+
				"already open and someone is paged, that context shapes the rest of "+
				"the investigation (you may be duplicating work, or the cause is "+
				"already known). "+
				"[Methodology: Incident context (pre-Step 0) — see docs/methodology.md]",
		),
		mcp.WithString("service",
			mcp.Description("Optional service name to filter incidents by (exact match). When omitted, returns all active incidents across the org."),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleListActiveIncidents,
	}
}

func (p *Provider) handleListActiveIncidents(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, activeIncidentsTimeout)
	defer cancel()

	serviceName := req.GetString("service", "")

	log := p.logger.With(slog.String("tool", "list_active_incidents"))
	log.Info("tool called", "service", serviceName)

	params := url.Values{
		"statuses[]": {"triggered", "acknowledged"},
		"limit":      {fmt.Sprintf("%d", activeIncidentsLimit)},
		"sort_by":    {"created_at:desc"},
	}

	var serviceLabel string
	if serviceName != "" {
		svc, err := p.client.FindServiceByName(ctx, serviceName)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("service lookup: %v", err)), nil
		}
		params.Add("service_ids[]", svc.ID)
		serviceLabel = svc.Name
	}

	incidents, err := p.client.ListIncidents(ctx, params)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("pagerduty API error: %v", err)), nil
	}

	return mcp.NewToolResultText(buildActiveIncidentsReport(serviceLabel, incidents)), nil
}

func buildActiveIncidentsReport(serviceLabel string, incidents []Incident) string {
	var b strings.Builder

	if serviceLabel != "" {
		fmt.Fprintf(&b, "## Active PagerDuty Incidents — `%s`\n\n", serviceLabel)
	} else {
		b.WriteString("## Active PagerDuty Incidents (all services)\n\n")
	}
	fmt.Fprintf(&b, "**Incidents returned:** %d\n\n", len(incidents))

	if len(incidents) == 0 {
		b.WriteString("_No active incidents._\n")
		return b.String()
	}

	b.WriteString("| # | Status | Urgency | Created | Service | Assignee | Title |\n")
	b.WriteString("|---|--------|---------|---------|---------|----------|-------|\n")
	for _, i := range incidents {
		assignee := "—"
		if len(i.Assignments) > 0 {
			assignee = i.Assignments[0].Assignee.Summary
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |\n",
			i.IncidentNumber,
			i.Status,
			i.Urgency,
			i.CreatedAt.UTC().Format(time.RFC3339),
			i.Service.Summary,
			assignee,
			truncate(i.Title, 70),
		)
	}

	b.WriteString("\nIf an incident matches the symptom you were about to investigate, defer to the assignee instead of duplicating work.\n")
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
