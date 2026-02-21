// Package kubernetes implements the Kubernetes provider for Rikugan.
//
// It uses client-go to connect to a Kubernetes cluster and exposes
// compound diagnostic tools via the Provider interface.
package kubernetes

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Zuful/rikugan/internal/provider"
)

// KubernetesProvider implements provider.Provider for Kubernetes backends.
type KubernetesProvider struct {
	client kubernetes.Interface
	logger *slog.Logger
}

// Option configures the KubernetesProvider.
type Option func(*options)

type options struct {
	kubeconfig string
	logger     *slog.Logger
}

// WithKubeconfig sets an explicit path to the kubeconfig file.
func WithKubeconfig(path string) Option {
	return func(o *options) { o.kubeconfig = path }
}

// WithLogger sets the structured logger for the provider.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// BuildClient creates a kubernetes.Interface using the standard config
// resolution chain: explicit path → KUBECONFIG env → ~/.kube/config → in-cluster.
func BuildClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := buildConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build k8s config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create k8s clientset: %w", err)
	}
	return cs, nil
}

// NewFromClient constructs a KubernetesProvider from a pre-built client.
func NewFromClient(client kubernetes.Interface, opts ...Option) *KubernetesProvider {
	o := &options{logger: slog.Default()}
	for _, fn := range opts {
		fn(o)
	}
	return &KubernetesProvider{client: client, logger: o.logger}
}

// New creates a KubernetesProvider. It resolves the kubeconfig in this order:
//  1. Explicit path via WithKubeconfig
//  2. KUBECONFIG environment variable
//  3. ~/.kube/config
//  4. In-cluster config (when running inside a pod)
func New(opts ...Option) (*KubernetesProvider, error) {
	o := &options{
		logger: slog.Default(),
	}
	for _, fn := range opts {
		fn(o)
	}

	cs, err := BuildClient(o.kubeconfig)
	if err != nil {
		return nil, err
	}

	o.logger.Info("kubernetes provider initialised")

	return &KubernetesProvider{
		client: cs,
		logger: o.logger,
	}, nil
}

// Name returns the provider identifier.
func (p *KubernetesProvider) Name() string { return "kubernetes" }

// Tools returns all MCP tools exposed by this provider.
func (p *KubernetesProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		p.diagnosePodTool(),
	}
}

// buildConfig resolves a *rest.Config using the priority chain described on New.
func buildConfig(explicit string) (*rest.Config, error) {
	// 1. Explicit path
	if explicit != "" {
		return clientcmd.BuildConfigFromFlags("", explicit)
	}

	// 2. KUBECONFIG env
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return clientcmd.BuildConfigFromFlags("", env)
	}

	// 3. ~/.kube/config
	home, err := os.UserHomeDir()
	if err == nil {
		defaultPath := filepath.Join(home, ".kube", "config")
		if _, statErr := os.Stat(defaultPath); statErr == nil {
			return clientcmd.BuildConfigFromFlags("", defaultPath)
		}
	}

	// 4. In-cluster
	return rest.InClusterConfig()
}
