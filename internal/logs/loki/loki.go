// Package loki implements the Loki provider for Rikugan.
//
// It queries the Loki HTTP API to search logs, retrieve contextual log
// windows, and detect anomalies in error rates, exposing synthesized
// reports via the Provider interface.
package loki

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for Loki backends.
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

// BuildClient creates a Loki REST client from the given URL.
// If empty, it falls back to LOKI_URL.
func BuildClient(baseURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = os.Getenv("LOKI_URL")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("loki URL not configured: set LOKI_URL")
	}
	return NewClient(baseURL), nil
}

// NewFromClient constructs a Provider from a pre-built client.
func NewFromClient(client *Client, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		client: client,
		logger: o.logger.With(slog.String("provider", "loki")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "loki" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.searchLogsTool(),
		p.getLogContextTool(),
		p.detectLogAnomaliesTool(),
	}
}
