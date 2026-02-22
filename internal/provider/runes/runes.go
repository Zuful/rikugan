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

// RunesProvider implements provider.Provider by correlating K8s and Prometheus data.
type RunesProvider struct {
	k8s  kubernetes.Interface
	prom promv1.API
	logger *slog.Logger
}

// Option configures the RunesProvider.
type Option func(*options)

type options struct {
	logger *slog.Logger
}

// WithLogger sets the structured logger for the provider.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// New creates a RunesProvider from pre-built K8s and Prometheus clients.
func New(k8s kubernetes.Interface, prom promv1.API, opts ...Option) *RunesProvider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &RunesProvider{
		k8s:    k8s,
		prom:   prom,
		logger: o.logger.With(slog.String("provider", "runes")),
	}
}

// Name returns the provider identifier.
func (p *RunesProvider) Name() string { return "runes" }

// Tools returns all MCP tools exposed by this provider.
func (p *RunesProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.sixEyesTool(),
	}
}
