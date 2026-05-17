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
	serviceIncidentsTimeout = 30 * time.Second
	defaultLookback         = 24 * time.Hour
	serviceIncidentsLimit   = 50
)

// listServiceIncidentsTool returns the ToolDefinition for list_service_incidents.
func (p *Provider) listServiceIncidentsTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_service_incidents",
		mcp.WithDescription(
			"List PagerDuty incidents (any status, including resolved) raised against "+
				"a service over a lookback window. Useful for spotting noisy services, "+
				"recurring issues, or whether an alert has fired repeatedly. Includes "+
				"already-resolved incidents — the goal is the historical pattern. "+
				"[Methodology: Incident context (pre-Step 0) — see docs/methodology.md]",
		),
		mcp.WithString("service",
			mcp.Required(),
			mcp.Description("Service name to filter incidents by (exact match)"),
		),
		mcp.WithString("since",
			mcp.Description("Lookback duration (e.g. \"24h\", \"3h\", \"7d\"). Default: 24h"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleListServiceIncidents,
	}
}

func (p *Provider) handleListServiceIncidents(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceIncidentsTimeout)
	defer cancel()

	serviceName, err := req.RequireString("service")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	lookback := defaultLookback
	if raw := req.GetString("since", ""); raw != "" {
		d, err := parseFlexibleDuration(raw)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid since: %v", err)), nil
		}
		lookback = d
	}

	log := p.logger.With(slog.String("tool", "list_service_incidents"))
	log.Info("tool called", "service", serviceName, "lookback", lookback.String())

	svc, err := p.client.FindServiceByName(ctx, serviceName)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("service lookup: %v", err)), nil
	}

	params := url.Values{
		"service_ids[]": {svc.ID},
		"since":         {time.Now().Add(-lookback).UTC().Format(time.RFC3339)},
		"limit":         {fmt.Sprintf("%d", serviceIncidentsLimit)},
		"sort_by":       {"created_at:desc"},
	}

	incidents, err := p.client.ListIncidents(ctx, params)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("pagerduty API error: %v", err)), nil
	}

	return mcp.NewToolResultText(buildServiceIncidentsReport(svc.Name, lookback, incidents)), nil
}

// parseFlexibleDuration accepts Go's standard durations ("1h30m") plus the
// "d" suffix for days ("7d") which PagerDuty users naturally reach for.
func parseFlexibleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h")
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(s)
}

func buildServiceIncidentsReport(serviceName string, lookback time.Duration, incidents []Incident) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## PagerDuty Incidents — `%s` (last %s)\n\n", serviceName, lookback)
	fmt.Fprintf(&b, "**Incidents returned:** %d\n\n", len(incidents))

	if len(incidents) == 0 {
		b.WriteString("_No incidents in this window — the service has been quiet._\n")
		return b.String()
	}

	// Quick counts by status to give the agent a one-glance picture.
	statusCounts := map[string]int{}
	for _, i := range incidents {
		statusCounts[i.Status]++
	}
	fmt.Fprintf(&b, "**Status breakdown:** ")
	first := true
	for _, st := range []string{"triggered", "acknowledged", "resolved"} {
		if c := statusCounts[st]; c > 0 {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", st, c)
			first = false
		}
	}
	b.WriteString("\n\n")

	b.WriteString("| # | Status | Urgency | Created | Title |\n")
	b.WriteString("|---|--------|---------|---------|-------|\n")
	for _, i := range incidents {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n",
			i.IncidentNumber,
			i.Status,
			i.Urgency,
			i.CreatedAt.UTC().Format(time.RFC3339),
			truncate(i.Title, 80),
		)
	}

	return b.String()
}
