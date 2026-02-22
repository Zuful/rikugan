package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v68/github"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const maxDiffChars = 10_000

// getCommitDiffTool returns the ToolDefinition for get_commit_diff.
func (p *GitHubProvider) getCommitDiffTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_commit_diff",
		mcp.WithDescription(
			"Fetch the full diff for a specific commit from a GitHub repository. "+
				"Returns commit metadata and per-file patches. "+
				"Useful for understanding exactly what code changed in a deploy.",
		),
		mcp.WithString("owner",
			mcp.Required(),
			mcp.Description("GitHub repository owner (user or organisation)"),
		),
		mcp.WithString("repo",
			mcp.Required(),
			mcp.Description("GitHub repository name"),
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

// handleGetCommitDiff is the MCP handler for get_commit_diff.
func (p *GitHubProvider) handleGetCommitDiff(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	owner, err := req.RequireString("owner")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	repo, err := req.RequireString("repo")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	sha, err := req.RequireString("commit_sha")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	log := p.logger.With(slog.String("tool", "get_commit_diff"))
	log.Info("tool called",
		"owner", owner,
		"repo", repo,
		"commit_sha", sha,
	)

	commit, _, err := p.client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("github API error: %v", err)), nil
	}

	report := buildCommitDiffReport(owner, repo, commit)
	return mcp.NewToolResultText(report), nil
}

func buildCommitDiffReport(owner, repo string, commit *gh.RepositoryCommit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Commit Diff\n\n")
	fmt.Fprintf(&b, "**Repository:** `%s/%s`\n\n", owner, repo)

	// Metadata
	sha := commit.GetSHA()
	author := ""
	if commit.GetAuthor() != nil {
		author = commit.GetAuthor().GetLogin()
	}
	if author == "" && commit.GetCommit().GetAuthor() != nil {
		author = commit.GetCommit().GetAuthor().GetName()
	}

	date := ""
	if commit.GetCommit().GetAuthor() != nil {
		date = commit.GetCommit().GetAuthor().GetDate().Format(time.RFC3339)
	}

	msg := ""
	if commit.GetCommit() != nil {
		msg = commit.GetCommit().GetMessage()
	}

	fmt.Fprintf(&b, "| Field | Value |\n")
	fmt.Fprintf(&b, "|-------|-------|\n")
	fmt.Fprintf(&b, "| SHA | `%s` |\n", sha)
	fmt.Fprintf(&b, "| Author | %s |\n", author)
	fmt.Fprintf(&b, "| Date | %s |\n", date)
	fmt.Fprintf(&b, "\n**Message:**\n```\n%s\n```\n\n", msg)

	fmt.Fprintf(&b, "**Files changed:** %d\n\n", len(commit.Files))

	// Per-file patches
	for _, f := range commit.Files {
		fmt.Fprintf(&b, "### `%s`\n\n", f.GetFilename())
		fmt.Fprintf(&b, "Status: **%s** | +%d -%d\n\n", f.GetStatus(), f.GetAdditions(), f.GetDeletions())
		if patch := f.GetPatch(); patch != "" {
			fmt.Fprintf(&b, "```diff\n%s\n```\n\n", patch)
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
