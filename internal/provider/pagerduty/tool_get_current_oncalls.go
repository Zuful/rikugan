package pagerduty

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const oncallsTimeout = 30 * time.Second

// getCurrentOncallsTool returns the ToolDefinition for get_current_oncalls.
func (p *Provider) getCurrentOncallsTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_current_oncalls",
		mcp.WithDescription(
			"List who is currently on-call across the org, grouped by escalation "+
				"policy and ordered by escalation level (primary first). Use this when "+
				"the investigation has surfaced something serious and you need to "+
				"know who to notify. Returns each policy's level-1 (primary) and "+
				"level-2 (secondary) responders. "+
				"[Methodology: Incident context — answers 'who do I notify?' after a finding — see docs/methodology.md]",
		),
		mcp.WithString("escalation_policy",
			mcp.Description("Optional escalation policy name substring to filter (PagerDuty matches substring on the policy summary)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetCurrentOncalls,
	}
}

func (p *Provider) handleGetCurrentOncalls(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, oncallsTimeout)
	defer cancel()

	policyFilter := req.GetString("escalation_policy", "")

	log := p.logger.With(slog.String("tool", "get_current_oncalls"))
	log.Info("tool called", "escalation_policy", policyFilter)

	params := url.Values{
		"earliest": {"true"}, // one entry per (policy, level) instead of every shift
		"limit":    {"100"},
	}

	oncalls, err := p.client.ListOncalls(ctx, params)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("pagerduty API error: %v", err)), nil
	}

	if policyFilter != "" {
		oncalls = filterOncallsByPolicy(oncalls, policyFilter)
	}

	return mcp.NewToolResultText(buildOncallsReport(policyFilter, oncalls)), nil
}

func filterOncallsByPolicy(oncalls []Oncall, substr string) []Oncall {
	substr = strings.ToLower(substr)
	var out []Oncall
	for _, o := range oncalls {
		if strings.Contains(strings.ToLower(o.EscalationPolicy.Summary), substr) {
			out = append(out, o)
		}
	}
	return out
}

func buildOncallsReport(filter string, oncalls []Oncall) string {
	var b strings.Builder

	b.WriteString("## Current PagerDuty On-Calls\n\n")
	if filter != "" {
		fmt.Fprintf(&b, "**Filter:** policy name contains `%s`  \n", filter)
	}
	fmt.Fprintf(&b, "**Entries returned:** %d\n\n", len(oncalls))

	if len(oncalls) == 0 {
		b.WriteString("_No on-call assignments match._\n")
		return b.String()
	}

	// Group by escalation policy.
	grouped := map[string][]Oncall{}
	for _, o := range oncalls {
		grouped[o.EscalationPolicy.Summary] = append(grouped[o.EscalationPolicy.Summary], o)
	}

	policyNames := make([]string, 0, len(grouped))
	for name := range grouped {
		policyNames = append(policyNames, name)
	}
	sort.Strings(policyNames)

	for _, name := range policyNames {
		entries := grouped[name]
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].EscalationLevel < entries[j].EscalationLevel
		})

		fmt.Fprintf(&b, "### %s\n\n", name)
		b.WriteString("| Level | User | Schedule | Until |\n")
		b.WriteString("|-------|------|----------|-------|\n")
		for _, e := range entries {
			schedule := "—"
			if e.Schedule != nil {
				schedule = e.Schedule.Summary
			}
			until := "—"
			if e.End != nil {
				until = e.End.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s |\n",
				e.EscalationLevel,
				e.User.Summary,
				schedule,
				until,
			)
		}
		b.WriteString("\n")
	}

	return b.String()
}
