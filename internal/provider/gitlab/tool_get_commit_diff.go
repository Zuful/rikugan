package gitlab

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/Zuful/rikugan/internal/provider"
)

const maxDiffChars = 10_000

// getCommitDiffTool returns the ToolDefinition for gitlab_get_commit_diff.
func (p *Provider) getCommitDiffTool() provider.ToolDefinition {
	tool := mcp.NewTool("gitlab_get_commit_diff",
		mcp.WithDescription(
			"Fetch the full diff for a specific commit from a GitLab project. "+
				"Returns commit metadata and per-file patches. "+
				"Useful for understanding exactly what code changed in a deploy.",
		),
		mcp.WithString("project_id",
			mcp.Required(),
			mcp.Description("GitLab project ID (numeric) or URL-encoded path (e.g. \"group/project\")"),
		),
		mcp.WithString("commit_sha",
			mcp.Required(),
			mcp.Description("Full or abbreviated commit SHA"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetCommitDiff,
	}
}

// handleGetCommitDiff is the MCP handler for gitlab_get_commit_diff.
func (p *Provider) handleGetCommitDiff(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sha, err := req.RequireString("commit_sha")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	log := p.logger.With(slog.String("tool", "gitlab_get_commit_diff"))
	log.Info("tool called",
		"project_id", projectID,
		"commit_sha", sha,
	)

	// Fetch commit metadata.
	commit, _, err := p.client.Commits.GetCommit(projectID, sha, nil, gl.WithContext(ctx))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("gitlab API error: %v", err)), nil
	}

	// Fetch diffs (separate endpoint in GitLab).
	diffs, _, err := p.client.Commits.GetCommitDiff(projectID, sha, &gl.GetCommitDiffOptions{}, gl.WithContext(ctx))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("gitlab API error fetching diff: %v", err)), nil
	}

	report := buildCommitDiffReport(projectID, commit, diffs)
	return mcp.NewToolResultText(report), nil
}

func buildCommitDiffReport(projectID string, commit *gl.Commit, diffs []*gl.Diff) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Commit Diff\n\n")
	fmt.Fprintf(&b, "**Project:** `%s`\n\n", projectID)

	// Metadata
	date := ""
	if commit.CommittedDate != nil {
		date = commit.CommittedDate.Format(time.RFC3339)
	}

	fmt.Fprintf(&b, "| Field | Value |\n")
	fmt.Fprintf(&b, "|-------|-------|\n")
	fmt.Fprintf(&b, "| SHA | `%s` |\n", commit.ID)
	fmt.Fprintf(&b, "| Author | %s |\n", commit.AuthorName)
	fmt.Fprintf(&b, "| Date | %s |\n", date)
	fmt.Fprintf(&b, "\n**Message:**\n```\n%s\n```\n\n", commit.Message)

	fmt.Fprintf(&b, "**Files changed:** %d\n\n", len(diffs))

	// Per-file patches
	for _, d := range diffs {
		fmt.Fprintf(&b, "### `%s`\n\n", diffFilename(d))
		fmt.Fprintf(&b, "New file: %t | Renamed: %t | Deleted: %t\n\n", d.NewFile, d.RenamedFile, d.DeletedFile)
		if d.Diff != "" {
			fmt.Fprintf(&b, "```diff\n%s\n```\n\n", d.Diff)
		}
	}

	// Truncation
	report := b.String()
	if len(report) > maxDiffChars {
		total := len(report)
		report = report[:maxDiffChars] + fmt.Sprintf("\n\n[Diff truncated — showing first %d characters of %d total]", maxDiffChars, total)
	}

	return report
}

// diffFilename returns the most relevant filename for a diff entry.
func diffFilename(d *gl.Diff) string {
	if d.NewPath != "" {
		return d.NewPath
	}
	return d.OldPath
}
