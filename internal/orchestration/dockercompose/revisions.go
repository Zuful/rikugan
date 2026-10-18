package dockercompose

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
		if at, ready := readyAt(&insp); ready {
			rev.Ready++
			if at.After(rev.ReadyAt) {
				rev.ReadyAt = at
			}
		}
	}
	out := make([]Revision, 0, len(byHash))
	for _, rev := range byHash {
		if rev.Ready < rev.Containers {
			rev.ReadyAt = time.Time{}
		}
		out = append(out, *rev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// readyAt reports whether the container is ready, and since when.
func readyAt(c *dockerInspect) (time.Time, bool) {
	st := c.State
	if !st.Running || st.Restarting || st.Paused {
		return time.Time{}, false
	}
	started := parseDockerTime(st.StartedAt)
	if st.Health == nil {
		return started, true
	}
	if st.Health.Status != "healthy" {
		return time.Time{}, false
	}
	return firstNonZero(passingSince(st.Health.Log), started), true
}

// passingSince returns the start of the trailing run of passing probes
// in the healthcheck log. Docker keeps only the last few probes, so for
// a long-healthy container this is the oldest probe on record, later
// than the true moment; for a rollout's fresh containers the log still
// holds the first passing probe.
func passingSince(log []healthProbe) time.Time {
	var since time.Time
	for i := len(log) - 1; i >= 0 && log[i].ExitCode == 0; i-- {
		since = parseDockerTime(log[i].Start)
	}
	return since
}

// ShortHash is the short form of a config-hash, as Sequence and
// messages show it.
func ShortHash(hash string) string {
	hash = strings.TrimPrefix(hash, "sha256:")
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
