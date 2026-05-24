package sentry

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	listIssuesTimeout  = 30 * time.Second
	defaultIssuesLimit = 20
	maxIssuesLimit     = 100
)

// listRecentIssuesTool returns the ToolDefinition for list_recent_issues.
func (p *Provider) listRecentIssuesTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_recent_issues",
		mcp.WithDescription(
			"List recent Sentry issues for a project, ordered by last-seen. "+
				"An issue is a deduplicated group of similar errors — 1000 occurrences of "+
				"the same exception collapse into one issue with a count and event metadata. "+
				"Useful for spotting active errors after a deploy or during an incident. "+
				"[Methodology: Errors signal of LEST (deduplicated, with release attribution) — see docs/methodology.md]",
		),
		mcp.WithString("project",
			mcp.Required(),
			mcp.Description("Sentry project slug (the URL slug, e.g. \"web-app\" or \"backend-api\")"),
		),
		mcp.WithString("query",
			mcp.Description("Sentry search query (e.g. \"is:unresolved level:error\"). Default: \"is:unresolved\""),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of issues to return (default 20, max 100)"),
			mcp.DefaultNumber(defaultIssuesLimit),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleListRecentIssues,
	}
}

func (p *Provider) handleListRecentIssues(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, listIssuesTimeout)
	defer cancel()

	project, err := req.RequireString("project")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	query := req.GetString("query", "")
	limit := req.GetInt("limit", defaultIssuesLimit)
	if limit <= 0 {
		limit = defaultIssuesLimit
	}
	if limit > maxIssuesLimit {
		limit = maxIssuesLimit
	}

	log := p.logger.With(slog.String("tool", "list_recent_issues"))
	log.Info("tool called",
		"project", project,
		"query", query,
		"limit", limit,
	)

	issues, err := p.client.ListProjectIssues(ctx, project, query, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("sentry API error: %v", err)), nil
	}

	return mcp.NewToolResultText(buildIssuesListReport(project, query, issues)), nil
}

func buildIssuesListReport(project, query string, issues []Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Sentry Issues — `%s`\n\n", project)
	if query != "" {
		fmt.Fprintf(&b, "**Query:** `%s`  \n", query)
	} else {
		b.WriteString("**Query:** `is:unresolved` (default)  \n")
	}
	fmt.Fprintf(&b, "**Issues returned:** %d\n\n", len(issues))

	if len(issues) == 0 {
		b.WriteString("_No issues match the query._\n")
		return b.String()
	}

	b.WriteString("| Short ID | Level | Count | Users | Last seen | Title |\n")
	b.WriteString("|----------|-------|-------|-------|-----------|-------|\n")
	for _, i := range issues {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %d | %s | %s |\n",
			i.ShortID,
			i.Level,
			i.Count,
			i.UserCount,
			i.LastSeen.UTC().Format(time.RFC3339),
			truncate(i.Title, 80),
		)
	}

	b.WriteString("\nUse `get_issue_detail` with the numeric issue ID to drill into a specific issue.\n")
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
