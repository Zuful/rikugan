package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/pkg/provider"
)

const opTimeout = 30 * time.Second

// Compile-time check that *ConceptProvider satisfies provider.Provider.
var _ provider.Provider = (*ConceptProvider)(nil)

// ConceptProvider is the concept-level tool registry for the alerts
// concept. It wraps the operator-selected provider.AlertsProvider
// implementation (alertmanager, sentry, or pagerduty) and exposes
// two technology-agnostic tools — list_active_alerts and
// get_alert_details — that route through the AlertsProvider interface
// and format the result as Markdown for MCP clients.
//
// Non-selected backends still register their own non-canonical tools
// directly (e.g. sentry's list_issues_for_release, pagerduty's
// get_current_oncalls) — only the concept-level routing is exclusive.
type ConceptProvider struct {
	impl     provider.AlertsProvider
	implName string
	logger   *slog.Logger
}

// NewConceptProvider wires the concept tools to a specific
// AlertsProvider implementation. implName is the backend's short
// label (e.g. "alertmanager") surfaced in report headers and logs.
func NewConceptProvider(impl provider.AlertsProvider, implName string, logger *slog.Logger) *ConceptProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConceptProvider{
		impl:     impl,
		implName: implName,
		logger:   logger.With(slog.String("provider", "alerts"), slog.String("backend", implName)),
	}
}

// Name returns the provider identifier surfaced to the dispatcher.
func (c *ConceptProvider) Name() string { return "alerts" }

// Tools returns the canonical Alerts concept tools.
func (c *ConceptProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		c.listActiveAlertsTool(),
		c.getAlertDetailsTool(),
	}
}

// ── Tool definitions ────────────────────────────────────────────────────────

func (c *ConceptProvider) listActiveAlertsTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_active_alerts",
		mcp.WithDescription(
			"List currently-active alerts via the configured alerts backend "+
				"(alertmanager / sentry / pagerduty — selected by ALERTS_PROVIDER). "+
				"The optional `service` parameter scopes the query; how it maps to "+
				"the backend is operator-dependent: alertmanager filters by the "+
				"`service` label, sentry treats it as a project slug, pagerduty "+
				"looks it up via FindServiceByName. "+
				"[Methodology: Incident context (pre-Step 0) — see docs/methodology.md]",
		),
		mcp.WithString("service", mcp.Description("Optional service / project / scope key — see description for backend-specific mapping.")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleListActiveAlerts}
}

func (c *ConceptProvider) getAlertDetailsTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_alert_details",
		mcp.WithDescription(
			"Fetch the full detail of a single alert via the configured alerts "+
				"backend. The `alert_id` is the backend-native identifier — for "+
				"alertmanager that's the alert fingerprint, for sentry the numeric "+
				"issue ID, for pagerduty the incident ID. "+
				"[Methodology: Incident context (pre-Step 0) — see docs/methodology.md]",
		),
		mcp.WithString("alert_id",
			mcp.Required(),
			mcp.Description("Backend-native alert identifier (fingerprint / issue ID / incident ID)."),
		),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleGetAlertDetails}
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *ConceptProvider) handleListActiveAlerts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	service := req.GetString("service", "")
	alerts, err := c.impl.ListActiveAlerts(ctx, service)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("alerts backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildListAlertsReport(c.implName, service, alerts)), nil
}

func (c *ConceptProvider) handleGetAlertDetails(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	id, err := req.RequireString("alert_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	alert, err := c.impl.GetAlertDetails(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("alerts backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildAlertDetailReport(c.implName, alert)), nil
}

// ── Report builders ─────────────────────────────────────────────────────────

func buildListAlertsReport(backend, service string, alerts []provider.Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Active Alerts\n\n")
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	if service != "" {
		fmt.Fprintf(&b, "**Service:** `%s`  \n", service)
	}
	fmt.Fprintf(&b, "**Alerts returned:** %d\n\n", len(alerts))

	if len(alerts) == 0 {
		b.WriteString("_No active alerts._\n")
		return b.String()
	}

	// Sort: severity (critical > error > warning > info > unknown), then start desc.
	sort.SliceStable(alerts, func(i, j int) bool {
		si := severityRank(alerts[i].Severity)
		sj := severityRank(alerts[j].Severity)
		if si != sj {
			return si > sj
		}
		return alerts[i].StartedAt.After(alerts[j].StartedAt)
	})

	b.WriteString("| Severity | State | Started | Service | ID | Title |\n")
	b.WriteString("|----------|-------|---------|---------|-----|-------|\n")
	for _, a := range alerts {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` | %s |\n",
			fallback(a.Severity, "—"),
			fallback(a.State, "—"),
			a.StartedAt.UTC().Format(time.RFC3339),
			fallback(a.Service, "—"),
			a.ID,
			escapePipe(a.Title),
		)
	}
	b.WriteString("\n_Use `get_alert_details` with the ID for backend-specific extras._\n")
	return b.String()
}

func buildAlertDetailReport(backend string, a provider.Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Alert `%s`\n\n", a.ID)
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	fmt.Fprintf(&b, "**Title:** %s\n\n", a.Title)

	fmt.Fprintf(&b, "| Field | Value |\n|-------|-------|\n")
	fmt.Fprintf(&b, "| Severity | %s |\n", fallback(a.Severity, "—"))
	fmt.Fprintf(&b, "| State | %s |\n", fallback(a.State, "—"))
	fmt.Fprintf(&b, "| Service | %s |\n", fallback(a.Service, "—"))
	fmt.Fprintf(&b, "| Source | %s |\n", a.Source)
	fmt.Fprintf(&b, "| Started | %s |\n", a.StartedAt.UTC().Format(time.RFC3339))
	if a.Description != "" {
		fmt.Fprintf(&b, "\n**Description:**\n\n%s\n", a.Description)
	}

	if len(a.Extras) > 0 {
		b.WriteString("\n### Backend-specific extras\n\n```json\n")
		out, err := json.MarshalIndent(a.Extras, "", "  ")
		if err == nil {
			b.Write(out)
		} else {
			fmt.Fprintf(&b, "{}")
		}
		b.WriteString("\n```\n")
	}
	return b.String()
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical", "fatal":
		return 4
	case "error":
		return 3
	case "warning", "warn":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func escapePipe(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
