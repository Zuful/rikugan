// Package prometheus implements the Prometheus provider for Rikugan.
//
// It uses the Prometheus HTTP API client to run PromQL queries and exposes
// structured markdown reports via the Provider interface.
package prometheus

import (
	"fmt"
	"log/slog"
	"os"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"github.com/Zuful/rikugan/internal/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for Prometheus backends.
type Provider struct {
	api    promv1.API
	logger *slog.Logger
}

// Option configures the Provider.
type Option func(*options)

type options struct {
	url    string
	logger *slog.Logger
}

// WithURL sets the Prometheus server URL.
func WithURL(url string) Option {
	return func(o *options) { o.url = url }
}

// WithLogger sets the structured logger for the provider.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// BuildAPI creates a promv1.API from a Prometheus server URL.
// If url is empty it falls back to PROMETHEUS_URL.
func BuildAPI(url string) (promv1.API, error) {
	if url == "" {
		url = os.Getenv("PROMETHEUS_URL")
	}
	if url == "" {
		return nil, fmt.Errorf("prometheus URL not configured: set PROMETHEUS_URL or use WithURL")
	}
	client, err := promapi.NewClient(promapi.Config{Address: url})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	return promv1.NewAPI(client), nil
}

// NewFromAPI constructs a Provider from a pre-built API.
func NewFromAPI(api promv1.API, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		api:    api,
		logger: o.logger.With(slog.String("provider", "prometheus")),
	}
}

// New creates a Provider. It resolves the URL in this order:
//  1. Explicit URL via WithURL
//  2. PROMETHEUS_URL environment variable
//  3. Error if neither is set
func New(opts ...Option) (*Provider, error) {
	o := &options{
		logger: slog.Default(),
	}
	for _, fn := range opts {
		fn(o)
	}

	api, err := BuildAPI(o.url)
	if err != nil {
		return nil, err
	}

	logger := o.logger.With(slog.String("provider", "prometheus"))
	logger.Info("provider initialised",
		slog.String("url", o.url),
	)

	return &Provider{
		api:    api,
		logger: logger,
	}, nil
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "prometheus" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.queryRangeTool(),
	}
}
