// Package pagerduty implements the PagerDuty provider for Rikugan.
//
// PagerDuty is incident-management, not observability — it answers
// orthogonal questions to LEST: who is on call, which incidents are
// already open, what alerts have fired recently. This provider exposes
// those signals as context an investigating agent should consult *before*
// running the LEST playbook.
package pagerduty

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for PagerDuty backends.
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

// BuildClient creates a PagerDuty REST client from the given API token.
// Falls back to PAGERDUTY_API_TOKEN. PagerDuty is SaaS-only; no base-URL
// override is exposed.
func BuildClient(token string) (*Client, error) {
	if token == "" {
		token = os.Getenv("PAGERDUTY_API_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("pagerduty API token not configured: set PAGERDUTY_API_TOKEN (create one at https://<subdomain>.pagerduty.com/api_keys)")
	}
	return NewClient(token), nil
}

// NewFromClient constructs a Provider from a pre-built client.
func NewFromClient(client *Client, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		client: client,
		logger: o.logger.With(slog.String("provider", "pagerduty")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "pagerduty" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.listActiveIncidentsTool(),
		p.listServiceIncidentsTool(),
		p.getCurrentOncallsTool(),
	}
}
