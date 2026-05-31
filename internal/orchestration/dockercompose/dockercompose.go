// Package dockercompose is the Docker Compose-backed slice of the
// orchestration concept. It implements all six methods of
// provider.OrchestrationProvider against a runtime that has no API
// server and no native rolling-update history concept — proof that
// the orchestration abstraction generalises beyond Kubernetes.
//
// Operators select this backend by setting ORCHESTRATION_PROVIDER=dockercompose
// before starting the unified rikugan binary or the per-concept
// rikugan-orchestration binary. COMPOSE_PROJECT_NAME picks the
// default Compose project when the per-call namespace argument is
// empty.
//
// One soft tear in the abstraction: GetServiceHistory has no native
// equivalent in Compose. Rather than break the interface, the method
// returns a documented degradation report listing currently-deployed
// images and pointing the operator at the container registry tag
// history, the compose-file git log, and CI build logs. See
// orchestrationprovider.go for the per-method assessment.
package dockercompose

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/Zuful/rikugan/pkg/provider"
)

// kindDockerComposeService is the value this provider sets on
// provider.Service.Kind. Concept-level tools that don't pass an
// explicit kind get this one as the default.
const kindDockerComposeService = "docker-compose-service"

// Compile-time check that *Provider satisfies provider.OrchestrationProvider —
// proof that the interface generalises beyond Kubernetes.
var _ provider.OrchestrationProvider = (*Provider)(nil)

// CommandRunner abstracts subprocess execution so the implementation
// can be unit-tested without spinning up a real docker daemon.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout []byte, err error)
}

// execRunner is the production CommandRunner — it shells out to the
// binary on PATH.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, errBuf.String())
	}
	return out.Bytes(), nil
}

// Provider is the Docker Compose-backed OrchestrationProvider. Project
// names the Compose project (the value of COMPOSE_PROJECT_NAME or the
// directory name when Compose was started); when empty the concept's
// `namespace` argument is forwarded as the project for each call.
type Provider struct {
	runner  CommandRunner
	project string // default project name; per-call namespace overrides this
	logger  *slog.Logger
}

// Option configures the Provider.
type Option func(*options)

type options struct {
	runner  CommandRunner
	project string
	logger  *slog.Logger
}

// WithRunner installs a custom CommandRunner — useful in tests.
func WithRunner(r CommandRunner) Option {
	return func(o *options) { o.runner = r }
}

// WithProject sets the default Compose project name used when the
// caller does not pass one as the namespace argument.
func WithProject(project string) Option {
	return func(o *options) { o.project = project }
}

// WithLogger installs a structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// New constructs a Docker Compose Provider with the given options.
func New(opts ...Option) *Provider {
	o := &options{
		runner: execRunner{},
		logger: slog.Default(),
	}
	for _, fn := range opts {
		fn(o)
	}
	return &Provider{
		runner:  o.runner,
		project: o.project,
		logger:  o.logger.With(slog.String("provider", "orchestration/docker-compose")),
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "orchestration/docker-compose" }

// Tools returns no backend-specific tools — every operation Docker
// Compose can express projects into the concept-level OrchestrationProvider
// interface. The concept provider in internal/orchestration is the
// sole entry point.
func (p *Provider) Tools() []provider.ToolDefinition { return nil }
