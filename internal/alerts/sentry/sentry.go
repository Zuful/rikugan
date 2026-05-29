// Package sentry implements the Sentry provider for Rikugan.
//
// It queries the Sentry REST API to surface application errors, exception
// detail, and release attribution, exposing synthesized Markdown reports
// via the Provider interface.
package sentry

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for Sentry backends.
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

// BuildClient creates a Sentry REST client from the given base URL, auth
// token, and organisation slug. Each argument falls back to its env var:
// SENTRY_URL (default https://sentry.io), SENTRY_AUTH_TOKEN, SENTRY_ORG.
// Token and org are required; failing those returns an error that main()
// surfaces as a warn-and-skip.
func BuildClient(baseURL, token, org string) (*Client, error) {
	if baseURL == "" {
		baseURL = os.Getenv("SENTRY_URL")
	}
	if baseURL == "" {
		baseURL = "https://sentry.io"
	}
	if token == "" {
		token = os.Getenv("SENTRY_AUTH_TOKEN")
	}
	if token == "" {
		return nil, fmt.Errorf("sentry auth token not configured: set SENTRY_AUTH_TOKEN (create one at <sentry-host>/settings/account/api/auth-tokens/)")
	}
	if org == "" {
		org = os.Getenv("SENTRY_ORG")
	}
	if org == "" {
		return nil, fmt.Errorf("sentry organisation slug not configured: set SENTRY_ORG (the URL slug shown in your Sentry dashboard)")
	}
	return NewClient(baseURL, token, org), nil
}

// NewFromClient constructs a Provider from a pre-built client.
func NewFromClient(client *Client, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		client: client,
		logger: o.logger.With(slog.String("provider", "sentry")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "sentry" }

// Tools returns the sentry-specific MCP tools that don't project
// into the concept-level Alerts interface — today, only the
// release-attribution view, which has no analog in AlertManager or
// PagerDuty. The canonical list_active_alerts / get_alert_details
// tools are exposed by internal/alerts.ConceptProvider, which routes
// through this provider's AlertsProvider methods when ALERTS_PROVIDER=sentry.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.listIssuesForReleaseTool(),
	}
}
