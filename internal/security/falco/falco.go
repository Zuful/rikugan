// Package falco implements the Falco provider for Rikugan.
//
// It queries a Falcosidekick REST API to surface runtime security and
// audit events, exposing synthesized reports via the Provider interface.
package falco

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for Falcosidekick backends.
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

// BuildClient creates a Falcosidekick REST client from the given URL.
// If empty, it falls back to FALCO_URL.
func BuildClient(baseURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = os.Getenv("FALCO_URL")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("falco URL not configured: set FALCO_URL")
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
		logger: o.logger.With(slog.String("provider", "falco")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "falco" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.getSecurityEventsTool(),
		p.analyzePodAuditTool(),
	}
}
