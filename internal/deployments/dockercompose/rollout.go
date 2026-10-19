// Package dockercompose is the Docker Compose slice of the deployments
// concept. Compose has no rollout controller and no history: a rollout
// is a change of the config-hash Compose stamps on a service's
// containers, and it is rebuilt from the containers as they are now.
package dockercompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Zuful/rikugan/internal/deployments/tracker"
	orchdc "github.com/Zuful/rikugan/internal/orchestration/dockercompose"
	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.DeploymentSource.
var _ provider.DeploymentSource = (*Provider)(nil)

// DefaultRolloutDeadline is compose_rollout_deadline's default: Compose
// has no progressDeadlineSeconds, so a revision whose containers are not
// all ready this long after it started is reported failed.
const DefaultRolloutDeadline = 10 * time.Minute

// DefaultPollInterval is how often WatchRollouts looks at the
// containers.
const DefaultPollInterval = 10 * time.Second

// Revisions is what the provider needs from the orchestration side;
// *orchdc.Provider implements it.
type Revisions interface {
	Revisions(ctx context.Context, svc provider.Service) ([]orchdc.Revision, error)
	ListServices(ctx context.Context, namespace string) ([]provider.Service, error)
}

// Provider is the Docker Compose DeploymentSource.
type Provider struct {
	compose  Revisions
	deadline time.Duration
	poll     time.Duration
	projects []string
	now      func() time.Time
	logger   *slog.Logger
}

// Option configures the Provider.
type Option func(*Provider)

// WithRolloutDeadline sets compose_rollout_deadline.
func WithRolloutDeadline(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.deadline = d
		}
	}
}

// WithPollInterval sets how often WatchRollouts polls.
func WithPollInterval(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.poll = d
		}
	}
}

// WithProjects scopes WatchRollouts to these Compose projects; without
// it, the Compose provider's default project is watched.
func WithProjects(projects ...string) Option {
	return func(p *Provider) { p.projects = projects }
}

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(p *Provider) { p.now = now }
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(p *Provider) {
		if l != nil {
			p.logger = l
		}
	}
}

// New builds the provider on the Compose orchestration provider.
func New(compose Revisions, opts ...Option) *Provider {
	p := &Provider{compose: compose, deadline: DefaultRolloutDeadline, poll: DefaultPollInterval, now: time.Now, logger: slog.Default()}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "deployments/docker-compose" }

// Tools implements provider.Provider.
func (p *Provider) Tools() []provider.ToolDefinition { return nil }

// LastRollout implements provider.DeploymentSource.
//
// The current revision is the config-hash of the service's most recently
// created containers. StartedAt is its first container's creation.
// The rollout is complete once every container of the revision is ready
// (healthy, or running without a healthcheck) and no container of an
// older revision is still running; CompletedAt is when the last of them
// became ready. Past compose_rollout_deadline without that, the rollout
// is failed. Compose keeps no history and numbers no revision, so
// Rollback is never set and Sequence is empty: Label shows the short
// config-hash.
func (p *Provider) LastRollout(ctx context.Context, svc provider.Service) (provider.Rollout, error) {
	revs, err := p.compose.Revisions(ctx, svc)
	if err != nil {
		return provider.Rollout{}, err
	}
	if len(revs) == 0 {
		return provider.Rollout{}, fmt.Errorf("compose service %s/%s has no container with a config-hash", svc.Namespace, svc.Name)
	}
	cur := revs[0]
	r := provider.Rollout{
		Service:   svc,
		Revision:  cur.Hash,
		State:     provider.RolloutInProgress,
		StartedAt: cur.Created,
	}
	if cur.Image != "" {
		r.Images = []string{cur.Image}
	}
	oldLive := 0
	for _, old := range revs[1:] {
		oldLive += old.Live
	}
	switch {
	case cur.Containers > 0 && cur.Ready == cur.Containers && oldLive == 0 && !cur.ReadyAt.IsZero():
		r.State = provider.RolloutComplete
		r.CompletedAt, r.CompletedAtEstimated = cur.ReadyAt, cur.ReadyAtEstimated
		if r.CompletedAt.Before(r.StartedAt) {
			r.CompletedAt = r.StartedAt
		}
	case p.now().Sub(cur.Created) > p.deadline:
		r.State = provider.RolloutFailed
		r.FailedAt = cur.Created.Add(p.deadline)
		r.FailureReason = fmt.Sprintf("%d of %d container(s) ready and %d of an older revision still running %s after the revision started (compose_rollout_deadline)",
			cur.Ready, cur.Containers, oldLive, p.deadline)
	}
	return r, nil
}

// WatchRollouts implements provider.DeploymentSource by polling: Compose
// has no controller to watch, so every poll interval each service of the
// watched projects is rebuilt by LastRollout, and the shared tracker
// reports what ended. CompletedAt is the readiness time of the
// containers, not the time of the poll that saw it.
func (p *Provider) WatchRollouts(ctx context.Context) (<-chan provider.Rollout, error) {
	projects := p.projects
	if len(projects) == 0 {
		projects = []string{""}
	}
	out := make(chan provider.Rollout)
	go func() {
		defer close(out)
		t := tracker.New()
		tick := time.NewTicker(p.poll)
		defer tick.Stop()
		for {
			if !p.pollOnce(ctx, projects, t, out) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return out, nil
}

// pollOnce observes every service once. It returns false once ctx is
// done.
func (p *Provider) pollOnce(ctx context.Context, projects []string, t *tracker.Tracker, out chan<- provider.Rollout) bool {
	seen := map[string]bool{}
	for _, project := range projects {
		services, err := p.compose.ListServices(ctx, project)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return false
			}
			p.logger.Warn("compose rollouts: cannot list services", slog.String("project", project), slog.String("error", err.Error()))
			// Keep what is tracked: a failing listing is not a deletion.
			for _, k := range t.Keys() {
				seen[k] = true
			}
			continue
		}
		for _, svc := range services {
			key := svc.Namespace + "/" + svc.Name
			seen[key] = true
			r, err := p.LastRollout(ctx, svc)
			if err != nil {
				p.logger.Debug("compose rollouts: no rollout", slog.String("service", key), slog.String("error", err.Error()))
				continue
			}
			for _, ev := range t.Observe(key, r) {
				select {
				case out <- ev:
				case <-ctx.Done():
					return false
				}
			}
		}
	}
	for _, k := range t.Keys() {
		if !seen[k] {
			t.Forget(k)
		}
	}
	return ctx.Err() == nil
}
