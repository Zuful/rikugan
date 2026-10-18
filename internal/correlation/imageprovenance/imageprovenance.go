// Package imageprovenance hosts get_image_provenance — the tool that
// walks a pod's container image tags and flags Git SHAs so the agent can
// pivot from "what's running" to "which commit shipped it".
//
// It is a correlation tool (docs/ontology.md): it reads deployment state
// (concept 5b, deployments) and answers in versioning terms (concept 5a),
// bridging the two rather than belonging to either. It reads the cluster
// directly for now; routing it through the deployments concept is part
// of the same follow-up as runes.
package imageprovenance

import (
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/pkg/provider"
)

// opTimeout bounds any single MCP tool call against the cluster.
const opTimeout = 30 * time.Second

// Compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for the versioning concept,
// using a Kubernetes cluster to read image metadata off live pods.
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
		logger: o.logger.With(slog.String("provider", "correlation/image-provenance")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "correlation/image-provenance" }

// Tools returns the MCP tools this provider exposes.
func (p *Provider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.getImageProvenanceTool(),
	}
}
