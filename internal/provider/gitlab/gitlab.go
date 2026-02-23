// Package gitlab implements the GitLab provider for Rikugan.
//
// It uses the GitLab REST API to fetch commit history and diffs, exposing
// structured markdown reports via the Provider interface. Supports both
// gitlab.com and self-hosted instances via GITLAB_URL.
package gitlab

import (
	"fmt"
	"log/slog"
	"os"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/Zuful/rikugan/internal/provider"
)

// GitLabProvider implements provider.Provider for GitLab backends.
type GitLabProvider struct {
	client *gl.Client
	logger *slog.Logger
}

// Option configures the GitLabProvider.
type Option func(*options)

type options struct {
	logger *slog.Logger
}

// WithLogger sets the structured logger for the provider.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// BuildClient creates a *gitlab.Client using the given token and base URL.
// If token is empty it falls back to GITLAB_TOKEN. If baseURL is empty it
// falls back to GITLAB_URL, then defaults to https://gitlab.com.
func BuildClient(token, baseURL string) (*gl.Client, error) {
	if token == "" {
		token = os.Getenv("GITLAB_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("gitlab token not configured: set GITLAB_TOKEN")
	}

	if baseURL == "" {
		baseURL = os.Getenv("GITLAB_URL")
	}

	opts := []gl.ClientOptionFunc{}
	if baseURL != "" {
		opts = append(opts, gl.WithBaseURL(baseURL))
	}

	client, err := gl.NewClient(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("create gitlab client: %w", err)
	}
	return client, nil
}

// NewFromClient constructs a GitLabProvider from a pre-built client.
func NewFromClient(client *gl.Client, opts ...Option) *GitLabProvider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &GitLabProvider{
		client: client,
		logger: o.logger.With(slog.String("provider", "gitlab")),
	}
}

// Name returns the provider identifier.
func (p *GitLabProvider) Name() string { return "gitlab" }

// Tools returns all MCP tools exposed by this provider.
func (p *GitLabProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.getRecentCommitsTool(),
		p.getCommitDiffTool(),
	}
}
