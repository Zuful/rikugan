package alertmanager

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

const opTimeout = 30 * time.Second

func (p *Provider) listActiveAlertsTool() provider.ToolDefinition {
	tool := mcp.NewTool("alertmanager_list_active_alerts",
		mcp.WithDescription(
			"List currently-active AlertManager alerts, optionally filtered by labels. "+
				"Returns a Markdown table grouped by severity. Pass label filters in "+
				"AlertManager's filter syntax (e.g. `severity=\"critical\"`). "+
				"[Methodology: Incident context (pre-Step 0) — see docs/methodology.md]",
		),
		mcp.WithString("filter",
			mcp.Description("Optional comma-separated label filters in AlertManager syntax (e.g. `severity=\"critical\",team=\"payments\"`). Defaults to no filter."),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleListActiveAlerts,
	}
}

func (p *Provider) handleListActiveAlerts(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	filterRaw := req.GetString("filter", "")
	var filters []string
	if filterRaw != "" {
		for _, f := range strings.Split(filterRaw, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				filters = append(filters, f)
			}
		}
	}

	log := p.logger.With(slog.String("tool", "alertmanager_list_active_alerts"))
	log.Info("tool called", "filters", filters)

	alerts, err := p.client.ListAlerts(ctx, filters)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("alertmanager API error: %v", err)), nil
	}

	return mcp.NewToolResultText(buildActiveAlertsReport(filters, alerts)), nil
}

func buildActiveAlertsReport(filters []string, alerts []Alert) string {
	var b strings.Builder

	b.WriteString("## Active AlertManager Alerts\n\n")
	if len(filters) > 0 {
		fmt.Fprintf(&b, "**Filters:** `%s`  \n", strings.Join(filters, ", "))
	}
	fmt.Fprintf(&b, "**Alerts returned:** %d\n\n", len(alerts))

	if len(alerts) == 0 {
		b.WriteString("_No active alerts._\n")
		return b.String()
	}

	// Order: severity (critical > warning > info > unknown) then by startsAt desc.
	sort.SliceStable(alerts, func(i, j int) bool {
		si := severityRank(alerts[i].Labels["severity"])
		sj := severityRank(alerts[j].Labels["severity"])
		if si != sj {
			return si > sj
		}
		return alerts[i].StartsAt.After(alerts[j].StartsAt)
	})

	b.WriteString("| Severity | State | Started | Alert | Service / Labels |\n")
	b.WriteString("|----------|-------|---------|-------|------------------|\n")
	for _, a := range alerts {
		alertname := a.Labels["alertname"]
		if alertname == "" {
			alertname = "(unnamed)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			defaultString(a.Labels["severity"], "—"),
			a.Status.State,
			a.StartsAt.UTC().Format(time.RFC3339),
			alertname,
			compactLabels(a.Labels),
		)
	}
	return b.String()
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func compactLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "alertname" || k == "severity" {
			continue // already surfaced as their own columns
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, ", ")
}
