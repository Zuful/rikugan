// Package argocd implements the Argo CD provider for Rikugan.
//
// It queries the Argo CD REST API to expose application sync status,
// health, and degraded-app discovery via the Provider interface.
package argocd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/internal/provider"
)

// Provider implements provider.Provider for Argo CD backends.
// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

type Provider struct {
	client *Client
	logger *slog.Logger
}

// Option configures the Provider.
type Option func(*options)

type options struct {
	logger *slog.Logger
}

// WithLogger sets the structured logger for the provider.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// BuildClient creates an Argo CD REST client from the given server URL
// and token. If empty, they fall back to ARGOCD_SERVER and ARGOCD_TOKEN.
func BuildClient(serverURL, token string) (*Client, error) {
	if serverURL == "" {
		serverURL = os.Getenv("ARGOCD_SERVER")
	}
	if serverURL == "" {
		return nil, fmt.Errorf("argocd server not configured: set ARGOCD_SERVER")
	}
	if token == "" {
		token = os.Getenv("ARGOCD_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("argocd token not configured: set ARGOCD_TOKEN")
	}
	return NewClient(serverURL, token), nil
}

// NewFromClient constructs an Provider from a pre-built client.
func NewFromClient(client *Client, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		client: client,
		logger: o.logger.With(slog.String("provider", "argocd")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "argocd" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.getAppStatusTool(),
		p.listDegradedAppsTool(),
	}
}
