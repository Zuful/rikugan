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

const getIssueTimeout = 30 * time.Second

// getIssueDetailTool returns the ToolDefinition for get_issue_detail.
func (p *Provider) getIssueDetailTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_issue_detail",
		mcp.WithDescription(
			"Fetch the full detail of a single Sentry issue: title, culprit, level, "+
				"count, first/last seen, the release that first introduced it, and the "+
				"failure metadata (type, value, function, filename). "+
				"Use this after list_recent_issues or list_issues_for_release to drill into a specific error. "+
				"[Methodology: Errors signal of LEST and Step 0 release attribution — see docs/methodology.md]",
		),
		mcp.WithString("issue_id",
			mcp.Required(),
			mcp.Description("Numeric Sentry issue ID (not the short ID like \"WEB-A12\"; the long numeric one)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetIssueDetail,
	}
}

func (p *Provider) handleGetIssueDetail(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, getIssueTimeout)
	defer cancel()

	issueID, err := req.RequireString("issue_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	log := p.logger.With(slog.String("tool", "get_issue_detail"))
	log.Info("tool called", "issue_id", issueID)

	issue, err := p.client.GetIssue(ctx, issueID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("sentry API error: %v", err)), nil
	}

	return mcp.NewToolResultText(buildIssueDetailReport(issue)), nil
}

func buildIssueDetailReport(i *Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Sentry Issue `%s` — %s\n\n", i.ShortID, i.Title)

	fmt.Fprintf(&b, "| Field | Value |\n")
	fmt.Fprintf(&b, "|-------|-------|\n")
	fmt.Fprintf(&b, "| Numeric ID | `%s` |\n", i.ID)
	fmt.Fprintf(&b, "| Level | %s |\n", i.Level)
	fmt.Fprintf(&b, "| Status | %s |\n", i.Status)
	fmt.Fprintf(&b, "| Project | %s |\n", i.Project.Slug)
	fmt.Fprintf(&b, "| Count | %s |\n", i.Count)
	fmt.Fprintf(&b, "| Users affected | %d |\n", i.UserCount)
	fmt.Fprintf(&b, "| First seen | %s |\n", i.FirstSeen.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Last seen | %s |\n", i.LastSeen.UTC().Format(time.RFC3339))
	if i.Culprit != "" {
		fmt.Fprintf(&b, "| Culprit | %s |\n", i.Culprit)
	}
	if i.FirstRelease != nil {
		fmt.Fprintf(&b, "| First release | `%s` (created %s) |\n",
			i.FirstRelease.Version,
			i.FirstRelease.Created.UTC().Format(time.RFC3339),
		)
	}
	if i.Permalink != "" {
		fmt.Fprintf(&b, "| Permalink | %s |\n", i.Permalink)
	}

	hasMeta := i.Metadata.Type != "" || i.Metadata.Value != "" || i.Metadata.Function != "" || i.Metadata.Filename != ""
	if hasMeta {
		b.WriteString("\n### Failure metadata\n\n")
		if i.Metadata.Type != "" {
			fmt.Fprintf(&b, "- **Type:** `%s`\n", i.Metadata.Type)
		}
		if i.Metadata.Value != "" {
			fmt.Fprintf(&b, "- **Value:** %s\n", i.Metadata.Value)
		}
		if i.Metadata.Function != "" {
			fmt.Fprintf(&b, "- **Function:** `%s`\n", i.Metadata.Function)
		}
		if i.Metadata.Filename != "" {
			fmt.Fprintf(&b, "- **File:** `%s`\n", i.Metadata.Filename)
		}
	}

	if i.FirstRelease != nil {
		fmt.Fprintf(&b, "\n_Hint: this issue first appeared in release `%s`. "+
			"Use `get_commit_diff` (github/gitlab) on the deploy that shipped that release "+
			"to inspect the changes._\n", i.FirstRelease.Version)
	}

	return b.String()
}
