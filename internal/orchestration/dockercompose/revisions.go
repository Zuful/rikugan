package dockercompose

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Revision is one config-hash of a Compose service, with its containers
// as they are now. The deployments concept rebuilds a rollout from it.
type Revision struct {
	Hash  string
	Image string
	// Created is the creation time of the revision's first container.
	Created time.Time
	// Containers counts the revision's containers, Ready those that are
	// running and healthy (or running, without a healthcheck), and Live
	// those still running or restarting.
	Containers, Ready, Live int
	// ReadyAt is when the last of the revision's containers became
	// ready, once all of them are: its start without a healthcheck, the
	// first probe of its current run of passing probes with one. Zero
	// while some container is not ready.
	ReadyAt time.Time
	// ReadyAtEstimated is set when, for some container, that first
	// passing probe has dropped out of Docker's healthcheck log: its
	// start, a lower bound that does not move, stands in for it.
	ReadyAtEstimated bool
}

// Revisions returns the service's revisions, newest first (by their
// first container's creation).
func (p *Provider) Revisions(ctx context.Context, svc provider.Service) ([]Revision, error) {
	containers, err := p.containersForService(ctx, svc)
	if err != nil {
		return nil, err
	}
	byHash := map[string]*Revision{}
	for _, c := range containers {
		insp, err := p.dockerInspect(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect container %s: %w", c.Name, err)
		}
		hash := insp.Config.Labels[labelConfigHash]
		if hash == "" {
			continue
		}
		rev := byHash[hash]
		if rev == nil {
			rev = &Revision{Hash: hash, Image: insp.Config.Image}
			byHash[hash] = rev
		}
		created := parseDockerTime(insp.Created)
		if rev.Created.IsZero() || created.Before(rev.Created) {
			rev.Created = created
		}
		rev.Containers++
		st := insp.State
		if st.Running || st.Restarting {
			rev.Live++
		}
		if at, estimated, ready := readyAt(&insp); ready {
			rev.Ready++
			if at.After(rev.ReadyAt) {
				rev.ReadyAt = at
			}
			rev.ReadyAtEstimated = rev.ReadyAtEstimated || estimated
		}
	}
	out := make([]Revision, 0, len(byHash))
	for _, rev := range byHash {
		if rev.Ready < rev.Containers {
			rev.ReadyAt, rev.ReadyAtEstimated = time.Time{}, false
		}
		out = append(out, *rev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// healthLogSize is how many probes Docker keeps in State.Health.Log.
const healthLogSize = 5

// readyAt reports whether the container is ready, since when, and
// whether that time is an estimate.
func readyAt(c *dockerInspect) (at time.Time, estimated, ready bool) {
	st := c.State
	if !st.Running || st.Restarting || st.Paused {
		return time.Time{}, false, false
	}
	started := parseDockerTime(st.StartedAt)
	if st.Health == nil {
		return started, false, true
	}
	if st.Health.Status != "healthy" {
		return time.Time{}, false, false
	}
	since, known := passingSince(st.Health.Log)
	if !known || since.IsZero() {
		return started, true, true
	}
	return since, false, true
}

// passingSince returns the start of the trailing run of passing probes
// in the healthcheck log, and whether that start is known. Docker keeps
// only the last few probes: once the log is full of passing probes, the
// run began before the oldest one on record, and taking that probe would
// move the time forward from one call to the next (about 2.5 minutes
// after a start with the default 30 s interval).
func passingSince(log []healthProbe) (time.Time, bool) {
	var since time.Time
	i := len(log) - 1
	for ; i >= 0 && log[i].ExitCode == 0; i-- {
		since = parseDockerTime(log[i].Start)
	}
	return since, i >= 0 || len(log) < healthLogSize
}
