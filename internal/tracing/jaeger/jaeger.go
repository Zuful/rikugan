// Package jaeger implements the Jaeger provider for Rikugan.
//
// It queries the Jaeger Query REST API to find traces and analyze span
// bottlenecks, exposing synthesized reports via the Provider interface.
package jaeger

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for Jaeger backends.
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

// BuildClient creates a Jaeger REST client from the given URL.
// If empty, it falls back to JAEGER_URL.
func BuildClient(baseURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = os.Getenv("JAEGER_URL")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("jaeger URL not configured: set JAEGER_URL")
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
		logger: o.logger.With(slog.String("provider", "jaeger")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "jaeger" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.findSlowTracesTool(),
		p.analyzeTraceTool(),
	}
}
