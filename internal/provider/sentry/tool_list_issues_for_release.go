package sentry

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
	releaseIssuesTimeout = 30 * time.Second
	defaultReleaseLimit  = 25
	maxReleaseLimit      = 100
)

// listIssuesForReleaseTool returns the ToolDefinition for list_issues_for_release.
func (p *Provider) listIssuesForReleaseTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_issues_for_release",
		mcp.WithDescription(
			"List unresolved Sentry issues first seen in a specific release. "+
				"Answers the question \"what did this deploy break?\" — every issue that "+
				"first appeared *with* this release version is surfaced, sorted by "+
				"recency. This is the most direct evidence link between a deploy and "+
				"runtime errors. "+
				"[Methodology: Step 0 (what changed) and Errors signal of LEST — see docs/methodology.md]",
		),
		mcp.WithString("version",
			mcp.Required(),
			mcp.Description("Sentry release version string (matches whatever your app sends as the release, e.g. a Git SHA, a semver tag, or a build ID)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of issues to return (default 25, max 100)"),
			mcp.DefaultNumber(defaultReleaseLimit),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleListIssuesForRelease,
	}
}

func (p *Provider) handleListIssuesForRelease(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseIssuesTimeout)
	defer cancel()

	version, err := req.RequireString("version")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit := req.GetInt("limit", defaultReleaseLimit)
	if limit <= 0 {
		limit = defaultReleaseLimit
	}
	if limit > maxReleaseLimit {
		limit = maxReleaseLimit
	}

	log := p.logger.With(slog.String("tool", "list_issues_for_release"))
	log.Info("tool called", "version", version, "limit", limit)

	issues, err := p.client.ListReleaseIssues(ctx, version, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("sentry API error: %v", err)), nil
	}

	return mcp.NewToolResultText(buildReleaseIssuesReport(version, issues)), nil
}

func buildReleaseIssuesReport(version string, issues []Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Sentry Issues introduced by release `%s`\n\n", version)
	fmt.Fprintf(&b, "**Issues returned:** %d\n\n", len(issues))

	if len(issues) == 0 {
		b.WriteString("_No unresolved issues first seen in this release — it looks clean from Sentry's perspective._\n")
		return b.String()
	}

	b.WriteString("| Short ID | Level | Count | Users | Project | Title |\n")
	b.WriteString("|----------|-------|-------|-------|---------|-------|\n")
	for _, i := range issues {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %d | %s | %s |\n",
			i.ShortID,
			i.Level,
			i.Count,
			i.UserCount,
			i.Project.Slug,
			truncate(i.Title, 80),
		)
	}

	b.WriteString("\nUse `get_issue_detail` with the numeric issue ID for full failure metadata.\n")
	return b.String()
}
