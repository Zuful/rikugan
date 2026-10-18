// Package kubernetes is the Kubernetes-backed provider for the
// orchestration concept. It hosts pod/service introspection tools
// (diagnose_pod, find_pods_by_service) that operate on a live cluster.
//
// The kubernetes.Interface is built once (via pkg/kubernetes) and the
// resulting client is shared with the image-provenance correlation provider so
// both packages exercise the same cluster without two parallel
// authentication chains.
package kubernetes

import (
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/pkg/provider"
)

// opTimeout bounds any single MCP tool call against the cluster. The
// concept-level handlers in internal/orchestration apply their own
// timeout; this one is for tools that still live in this package
// (find_pods_by_service_name).
const opTimeout = 30 * time.Second

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

// Tools returns the kubernetes-specific MCP tools that don't project
// into the concept-level Orchestration interface. find_pods_by_service_name
// stays here because it's a discovery helper specific to how kubernetes
// labels pods — Docker Compose service names map 1:1 to container
// names and need no analogous resolver. The compound diagnose_pod tool
// was removed in étape 3: callers now compose describe_service +
// get_service_state + get_recent_events (concept tools) explicitly.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.findPodsByServiceTool(),
	}
}
