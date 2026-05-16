// Package runes implements the Runes integration provider for Rikugan.
//
// It correlates Kubernetes pod diagnostics with Prometheus metrics to
// produce unified incident analysis via the six_eyes compound tool.
package runes

import (
	"log/slog"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/internal/provider"
)

// Provider implements provider.Provider by correlating K8s and Prometheus data.
// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

type Provider struct {
	k8s  kubernetes.Interface
	prom promv1.API
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

// New creates a Provider from pre-built K8s and Prometheus clients.
func New(k8s kubernetes.Interface, prom promv1.API, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		k8s:    k8s,
		prom:   prom,
		logger: o.logger.With(slog.String("provider", "runes")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "runes" }

// Tools returns all MCP tools exposed by this provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.sixEyesTool(),
	}
}
