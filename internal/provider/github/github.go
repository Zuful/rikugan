// Package github implements the GitHub provider for Rikugan.
//
// It uses the GitHub REST API to fetch commit history and diffs, exposing
// structured markdown reports via the Provider interface.
package github

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	gh "github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/Zuful/rikugan/internal/provider"
)

// GitHubProvider implements provider.Provider for GitHub backends.
type GitHubProvider struct {
	client *gh.Client
	logger *slog.Logger
}

// Option configures the GitHubProvider.
type Option func(*options)

type options struct {
	logger *slog.Logger
}

// WithLogger sets the structured logger for the provider.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// BuildClient creates a *github.Client using the given token.
// If token is empty it falls back to GITHUB_TOKEN.
func BuildClient(token string) (*gh.Client, error) {
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("github token not configured: set GITHUB_TOKEN")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return gh.NewClient(tc), nil
}

// NewFromClient constructs a GitHubProvider from a pre-built client.
func NewFromClient(client *gh.Client, opts ...Option) *GitHubProvider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &GitHubProvider{
		client: client,
		logger: o.logger.With(slog.String("provider", "github")),
	}
}

// Name returns the provider identifier.
func (p *GitHubProvider) Name() string { return "github" }

// Tools returns all MCP tools exposed by this provider.
func (p *GitHubProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.getRecentCommitsTool(),
		p.getCommitDiffTool(),
	}
}
