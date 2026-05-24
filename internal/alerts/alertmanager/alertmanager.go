// Package alertmanager is the AlertManager-backed slice of the alerts
// concept. AlertManager is the Prometheus stack's alert routing daemon;
// this provider lets agents list and inspect currently-firing alerts.
//
// Étape 2 scaffold — exposes a single tool (alertmanager_list_active_alerts)
// that wraps GET /api/v2/alerts. The full AlertsProvider implementation
// (with Ack/Resolve/Silence) lands in Étape 3 when the concept
// interface is finalised.
package alertmanager

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for AlertManager backends.
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

// BuildClient creates an AlertManager REST client from the given URL.
// If empty, it falls back to ALERTMANAGER_URL.
func BuildClient(baseURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = os.Getenv("ALERTMANAGER_URL")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("alertmanager URL not configured: set ALERTMANAGER_URL")
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
		logger: o.logger.With(slog.String("provider", "alerts/alertmanager")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "alerts/alertmanager" }

// Tools returns the MCP tools this provider exposes.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.listActiveAlertsTool(),
	}
}
