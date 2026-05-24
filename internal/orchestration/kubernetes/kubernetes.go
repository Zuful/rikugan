// Package kubernetes is the Kubernetes-backed provider for the
// orchestration concept. It hosts pod/service introspection tools
// (diagnose_pod, find_pods_by_service) that operate on a live cluster.
//
// The kubernetes.Interface is built once (via pkg/kubernetes) and the
// resulting client is shared with the versioning/kubernetes provider so
// both packages exercise the same cluster without two parallel
// authentication chains.
package kubernetes

import (
	"log/slog"

	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for the orchestration concept,
// backed by a real Kubernetes cluster.
type Provider struct {
	client kubernetes.Interface
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

// NewFromClient constructs a Provider from a pre-built Kubernetes client.
func NewFromClient(client kubernetes.Interface, opts ...Option) *Provider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		client: client,
		logger: o.logger.With(slog.String("provider", "orchestration/kubernetes")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "orchestration/kubernetes" }

// Tools returns the MCP tools this provider exposes — the
// orchestration-shaped subset of what the old kubernetes provider
// shipped.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.diagnosePodTool(),
		p.findPodsByServiceTool(),
	}
}
