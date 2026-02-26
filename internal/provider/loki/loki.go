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

	"github.com/Zuful/rikugan/internal/provider"
)

// LokiProvider implements provider.Provider for Loki backends.
type LokiProvider struct {
	client *Client
	logger *slog.Logger
}

// Option configures the LokiProvider.
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

// NewFromClient constructs a LokiProvider from a pre-built client.
func NewFromClient(client *Client, opts ...Option) *LokiProvider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &LokiProvider{
		client: client,
		logger: o.logger.With(slog.String("provider", "loki")),
	}
}

// Name returns the provider identifier.
func (p *LokiProvider) Name() string { return "loki" }

// Tools returns all MCP tools exposed by this provider.
func (p *LokiProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.searchLogsTool(),
		p.getLogContextTool(),
		p.detectLogAnomaliesTool(),
	}
}
