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

const opTimeout = 30 * time.Second

// getRecentCommitsTool returns the ToolDefinition for get_recent_commits.
func (p *Provider) getRecentCommitsTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_recent_commits",
		mcp.WithDescription(
			"Fetch the most recent commits from a GitHub repository. "+
				"Returns a Markdown table with commit hash, author, date, and message. "+
				"Useful for correlating code changes with runtime incidents.",
		),
		mcp.WithString("owner",
			mcp.Required(),
			mcp.Description("GitHub repository owner (user or organisation)"),
		),
		mcp.WithString("repo",
			mcp.Required(),
			mcp.Description("GitHub repository name"),
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

// handleGetRecentCommits is the MCP handler for get_recent_commits.
func (p *Provider) handleGetRecentCommits(
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

	branch, err := req.RequireString("branch")
	if err != nil {
		branch = "main"
	}

	log := p.logger.With(slog.String("tool", "get_recent_commits"))
	log.Info("tool called",
		"owner", owner,
		"repo", repo,
		"branch", branch,
	)

	commits, _, err := p.client.Repositories.ListCommits(ctx, owner, repo, &gh.CommitsListOptions{
		SHA:         branch,
		ListOptions: gh.ListOptions{PerPage: 10},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("github API error: %v", err)), nil
	}

	report := buildRecentCommitsReport(owner, repo, branch, commits)
	return mcp.NewToolResultText(report), nil
}

func buildRecentCommitsReport(owner, repo, branch string, commits []*gh.RepositoryCommit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Recent Commits\n\n")
	fmt.Fprintf(&b, "**Repository:** `%s/%s`  \n", owner, repo)
	fmt.Fprintf(&b, "**Branch:** `%s`\n\n", branch)

	if len(commits) == 0 {
		b.WriteString("_No commits found._\n")
		return b.String()
	}

	b.WriteString("| Hash | Author | Date | Message |\n")
	b.WriteString("|------|--------|------|---------|\n")

	for _, c := range commits {
		sha := c.GetSHA()
		if len(sha) > 7 {
			sha = sha[:7]
		}

		author := ""
		if c.GetAuthor() != nil {
			author = c.GetAuthor().GetLogin()
		}
		if author == "" && c.GetCommit().GetAuthor() != nil {
			author = c.GetCommit().GetAuthor().GetName()
		}

		date := ""
		if c.GetCommit().GetAuthor() != nil {
			date = c.GetCommit().GetAuthor().GetDate().Format(time.RFC3339)
		}

		msg := ""
		if c.GetCommit() != nil {
			msg = c.GetCommit().GetMessage()
			// Use only first line of commit message.
			if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
				msg = msg[:idx]
			}
		}

		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", sha, author, date, msg)
	}

	return b.String()
}
