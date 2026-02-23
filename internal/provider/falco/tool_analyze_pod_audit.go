package falco

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

// analyzePodAuditTool returns the ToolDefinition for analyze_pod_audit.
func (p *FalcoProvider) analyzePodAuditTool() provider.ToolDefinition {
	tool := mcp.NewTool("analyze_pod_audit",
		mcp.WithDescription(
			"Analyze Falco security events for a specific pod over the last 24 hours. "+
				"Returns a synthesized summary grouped by rule (e.g. \"3x Terminal shell in container\") "+
				"rather than raw events, protecting the LLM context window. "+
				"Useful for detecting anomalous runtime behavior, manual intervention, or security breaches.",
		),
		mcp.WithString("pod_name",
			mcp.Required(),
			mcp.Description("Pod name to analyze audit events for"),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace (recommended for precision)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleAnalyzePodAudit,
	}
}

// handleAnalyzePodAudit is the MCP handler for analyze_pod_audit.
func (p *FalcoProvider) handleAnalyzePodAudit(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	podName, err := req.RequireString("pod_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	namespace, _ := req.RequireString("namespace")

	log := p.logger.With(slog.String("tool", "analyze_pod_audit"))
	log.Info("tool called",
		"pod_name", podName,
		"namespace", namespace,
	)

	// Fetch all events in the last 24h (no severity filter — we want the full picture).
	events, err := p.client.ListEvents(ctx, "", defaultLookback)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("falcosidekick API error: %v", err)), nil
	}

	// Filter to the target pod.
	filtered := filterEvents(events, namespace, podName)

	report := buildPodAuditReport(podName, namespace, filtered)
	return mcp.NewToolResultText(report), nil
}

// ── Grouping and analysis ───────────────────────────────────────────────────

type ruleGroup struct {
	Rule      string
	Priority  string
	Count     int
	FirstSeen time.Time
	LastSeen  time.Time
	Sample    string // representative output message
}

// priorityRank maps Falco priorities to a numeric rank for sorting.
func priorityRank(p string) int {
	switch strings.ToLower(p) {
	case "critical":
		return 0
	case "error":
		return 1
	case "warning":
		return 2
	case "notice":
		return 3
	case "informational", "info":
		return 4
	case "debug":
		return 5
	default:
		return 6
	}
}

func groupEvents(events []Event) []ruleGroup {
	m := map[string]*ruleGroup{}
	for _, e := range events {
		g, ok := m[e.Rule]
		if !ok {
			g = &ruleGroup{
				Rule:      e.Rule,
				Priority:  e.Priority,
				FirstSeen: e.Time,
				LastSeen:  e.Time,
				Sample:    e.Output,
			}
			m[e.Rule] = g
		}
		g.Count++
		if e.Time.Before(g.FirstSeen) {
			g.FirstSeen = e.Time
		}
		if e.Time.After(g.LastSeen) {
			g.LastSeen = e.Time
		}
		// Keep the highest severity seen for this rule.
		if priorityRank(e.Priority) < priorityRank(g.Priority) {
			g.Priority = e.Priority
		}
	}

	groups := make([]ruleGroup, 0, len(m))
	for _, g := range m {
		groups = append(groups, *g)
	}

	// Sort: severity descending, then count descending.
	sort.Slice(groups, func(i, j int) bool {
		ri, rj := priorityRank(groups[i].Priority), priorityRank(groups[j].Priority)
		if ri != rj {
			return ri < rj
		}
		return groups[i].Count > groups[j].Count
	})

	return groups
}

func buildPodAuditReport(podName, namespace string, events []Event) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Pod Audit Analysis\n\n")
	fmt.Fprintf(&b, "**Pod:** `%s`\n", podName)
	if namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`\n", namespace)
	}
	fmt.Fprintf(&b, "**Window:** last 24 hours  \n")
	fmt.Fprintf(&b, "**Total events:** %d\n\n", len(events))

	if len(events) == 0 {
		b.WriteString("_No security events found for this pod. The pod appears clean from a runtime security perspective._\n")
		return b.String()
	}

	groups := groupEvents(events)

	// Summary sentence.
	critCount := 0
	for _, g := range groups {
		if priorityRank(g.Priority) <= 1 { // Critical or Error
			critCount += g.Count
		}
	}
	fmt.Fprintf(&b, "**%d unique rules triggered** across %d events.", len(groups), len(events))
	if critCount > 0 {
		fmt.Fprintf(&b, " **%d event(s) at Critical/Error severity.**", critCount)
	}
	b.WriteString("\n\n")

	// Grouped table.
	b.WriteString("| Severity | Rule | Count | First Seen | Last Seen |\n")
	b.WriteString("|----------|------|-------|------------|----------|\n")

	for _, g := range groups {
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n",
			g.Priority,
			g.Rule,
			g.Count,
			g.FirstSeen.UTC().Format(time.RFC3339),
			g.LastSeen.UTC().Format(time.RFC3339),
		)
	}
	b.WriteString("\n")

	// Sample outputs for each rule (context for the LLM).
	fmt.Fprintf(&b, "### Sample Outputs\n\n")
	for _, g := range groups {
		sample := g.Sample
		if len(sample) > 200 {
			sample = sample[:197] + "..."
		}
		fmt.Fprintf(&b, "**%s** (%dx):\n> %s\n\n", g.Rule, g.Count, sample)
	}

	return b.String()
}
