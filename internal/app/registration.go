package app

import (
	"fmt"
	"log/slog"

	"github.com/Zuful/rikugan/internal/security/falco"
	"github.com/Zuful/rikugan/internal/tracing/jaeger"
	"github.com/Zuful/rikugan/internal/versioning/argocd"
	"github.com/Zuful/rikugan/internal/versioning/github"
	"github.com/Zuful/rikugan/internal/versioning/gitlab"
	"github.com/Zuful/rikugan/pkg/dispatcher"
	"github.com/Zuful/rikugan/pkg/provider"
)

// optionalProvider couples a human-readable name with a closure that
// builds the provider. The closure isolates each backend's signature
// quirks (e.g. gitlab.BuildClient takes 2 args, sentry takes 3) so the
// registration loop in Setup stays uniform.
type optionalProvider struct {
	name  string
	setup func() (provider.Provider, error)
}

// optionalProviders returns the static list of provider factories. The
// logger is captured by every closure so a successfully-built provider
// gets the same structured logger as everything else.
//
// Adding a new optional provider here is a 5-line slice entry — the
// registration loop and the warn-and-skip semantics are shared.
func optionalProviders(logger *slog.Logger) []optionalProvider {
	return []optionalProvider{
		{"github", func() (provider.Provider, error) {
			c, err := github.BuildClient("")
			if err != nil {
				return nil, err
			}
			return github.NewFromClient(c, github.WithLogger(logger)), nil
		}},
		{"gitlab", func() (provider.Provider, error) {
			c, err := gitlab.BuildClient("", "")
			if err != nil {
				return nil, err
			}
			return gitlab.NewFromClient(c, gitlab.WithLogger(logger)), nil
		}},
		{"argocd", func() (provider.Provider, error) {
			c, err := argocd.BuildClient("", "")
			if err != nil {
				return nil, err
			}
			return argocd.NewFromClient(c, argocd.WithLogger(logger)), nil
		}},
		{"jaeger", func() (provider.Provider, error) {
			c, err := jaeger.BuildClient("")
			if err != nil {
				return nil, err
			}
			return jaeger.NewFromClient(c, jaeger.WithLogger(logger)), nil
		}},
		{"falco", func() (provider.Provider, error) {
			c, err := falco.BuildClient("")
			if err != nil {
				return nil, err
			}
			return falco.NewFromClient(c, falco.WithLogger(logger)), nil
		}},
	}
}

// registerOptional runs setup() and registers the resulting provider.
// A setup failure (typically a missing env var) logs a warning and
// returns nil — startup proceeds without that provider. A dispatcher-
// side registration error is fatal and returned wrapped.
func registerOptional(
	d *dispatcher.Dispatcher,
	logger *slog.Logger,
	name string,
	setup func() (provider.Provider, error),
) error {
	p, err := setup()
	if err != nil {
		logger.Warn(name+" provider not available, skipping",
			slog.String("reason", err.Error()),
		)
		return nil
	}
	if err := d.Register(p); err != nil {
		return fmt.Errorf("register %s provider: %w", name, err)
	}
	return nil
}
