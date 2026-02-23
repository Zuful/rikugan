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

const opTimeout = 30 * time.Second

// getRecentCommitsTool returns the ToolDefinition for gitlab_get_recent_commits.
func (p *GitLabProvider) getRecentCommitsTool() provider.ToolDefinition {
	tool := mcp.NewTool("gitlab_get_recent_commits",
		mcp.WithDescription(
			"Fetch the most recent commits from a GitLab project. "+
				"Returns a Markdown table with commit hash, author, date, and message. "+
				"Useful for correlating code changes with runtime incidents.",
		),
		mcp.WithString("project_id",
			mcp.Required(),
			mcp.Description("GitLab project ID (numeric) or URL-encoded path (e.g. \"group/project\")"),
		),
		mcp.WithString("branch",
			mcp.Description("Branch name to list commits from (default: \"main\")"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetRecentCommits,
	}
}

// handleGetRecentCommits is the MCP handler for gitlab_get_recent_commits.
func (p *GitLabProvider) handleGetRecentCommits(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	branch, err := req.RequireString("branch")
	if err != nil {
		branch = "main"
	}

	log := p.logger.With(slog.String("tool", "gitlab_get_recent_commits"))
	log.Info("tool called",
		"project_id", projectID,
		"branch", branch,
	)

	commits, _, err := p.client.Commits.ListCommits(projectID, &gl.ListCommitsOptions{
		RefName:     &branch,
		ListOptions: gl.ListOptions{PerPage: 10},
	}, gl.WithContext(ctx))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("gitlab API error: %v", err)), nil
	}

	report := buildRecentCommitsReport(projectID, branch, commits)
	return mcp.NewToolResultText(report), nil
}

func buildRecentCommitsReport(projectID, branch string, commits []*gl.Commit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Recent Commits\n\n")
	fmt.Fprintf(&b, "**Project:** `%s`  \n", projectID)
	fmt.Fprintf(&b, "**Branch:** `%s`\n\n", branch)

	if len(commits) == 0 {
		b.WriteString("_No commits found._\n")
		return b.String()
	}

	b.WriteString("| Hash | Author | Date | Message |\n")
	b.WriteString("|------|--------|------|---------|\n")

	for _, c := range commits {
		sha := c.ShortID

		author := c.AuthorName

		date := ""
		if c.CommittedDate != nil {
			date = c.CommittedDate.Format(time.RFC3339)
		}

		msg := c.Title
		// Title is already the first line, but guard against multi-line.
		if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
			msg = msg[:idx]
		}

		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", sha, author, date, msg)
	}

	return b.String()
}
