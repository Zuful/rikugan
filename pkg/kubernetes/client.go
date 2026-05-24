// Package kubernetes hosts the shared Kubernetes client builder so that
// every concept-level provider that needs a kubernetes.Interface (today
// orchestration and versioning, tomorrow logs/metrics via kubectl) can
// reuse the same auth-resolution chain.
package kubernetes

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// BuildClient creates a kubernetes.Interface using the standard config
// resolution chain: explicit path → KUBECONFIG env → in-cluster →
// ~/.kube/config. It logs which auth method was used at INFO level.
func BuildClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, method, err := buildConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build k8s config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create k8s clientset: %w", err)
	}
	slog.Info("kubernetes client initialised", slog.String("auth_method", method))
	return cs, nil
}

// buildConfig resolves a *rest.Config and returns the auth method used.
// Resolution order: explicit path → KUBECONFIG env → in-cluster → ~/.kube/config.
func buildConfig(explicit string) (*rest.Config, string, error) {
	// 1. Explicit path
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return nil, "", fmt.Errorf("explicit kubeconfig %q: %w", explicit, err)
		}
		cfg, err := clientcmd.BuildConfigFromFlags("", explicit)
		if err != nil {
			return nil, "", err
		}
		return cfg, "explicit:" + explicit, nil
	}

	// 2. KUBECONFIG env
	if env := os.Getenv("KUBECONFIG"); env != "" {
		// #nosec G703 -- KUBECONFIG is the canonical Kubernetes env var
		// for pointing at a kubeconfig file; reading the path the user
		// set themselves is the intended behaviour.
		if _, err := os.Stat(env); err != nil {
			return nil, "", fmt.Errorf("KUBECONFIG=%q: %w", env, err)
		}
		cfg, err := clientcmd.BuildConfigFromFlags("", env)
		if err != nil {
			return nil, "", err
		}
		return cfg, "env:KUBECONFIG", nil
	}

	// 3. In-cluster (service account token mount)
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, "in-cluster", nil
	}

	// 4. ~/.kube/config
	home, err := os.UserHomeDir()
	if err == nil {
		defaultPath := filepath.Join(home, ".kube", "config")
		if _, statErr := os.Stat(defaultPath); statErr == nil {
			cfg, cfgErr := clientcmd.BuildConfigFromFlags("", defaultPath)
			if cfgErr != nil {
				return nil, "", cfgErr
			}
			return cfg, "kubeconfig:" + defaultPath, nil
		}
	}

	return nil, "", fmt.Errorf("no kubernetes configuration found: set KUBECONFIG, provide a kubeconfig path, or run inside a cluster")
}
