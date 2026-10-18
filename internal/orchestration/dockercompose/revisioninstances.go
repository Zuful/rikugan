package dockercompose

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// labelConfigHash is the hash Compose stamps on every container of a
// service from the service's resolved configuration. A container is
// recreated, with a new hash, whenever that configuration changes
// (image, env, command…), which makes it Compose's equivalent of the
// Kubernetes pod-template-hash.
const labelConfigHash = "com.docker.compose.config-hash"

// GetRevisionInstances returns the containers of one revision of a
// Compose service. revision is a config-hash; when empty, the hash of
// the service's most recently created container is used.
//
// Docker keeps only a container's last termination (State.ExitCode,
// FinishedAt, OOMKilled), so Terminations holds at most one entry per
// container; RestartCount carries the full count.
func (p *Provider) GetRevisionInstances(ctx context.Context, svc provider.Service, revision string) ([]provider.Instance, error) {
	containers, err := p.containersForService(ctx, svc)
	if err != nil {
		return nil, err
	}

	inspected := make([]dockerInspect, 0, len(containers))
	for _, c := range containers {
		insp, err := p.dockerInspect(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect container %s: %w", c.Name, err)
		}
		inspected = append(inspected, insp)
	}

	if revision == "" {
		revision = newestConfigHash(inspected)
		if revision == "" {
			return nil, nil
		}
	}

	out := make([]provider.Instance, 0, len(inspected))
	for i := range inspected {
		if inspected[i].Config.Labels[labelConfigHash] != revision {
			continue
		}
		out = append(out, containerInstance(&inspected[i], revision))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func newestConfigHash(containers []dockerInspect) string {
	var hash string
	var newest time.Time
	for i := range containers {
		created := parseDockerTime(containers[i].Created)
		if h := containers[i].Config.Labels[labelConfigHash]; h != "" && (hash == "" || created.After(newest)) {
			hash, newest = h, created
		}
	}
	return hash
}

func containerInstance(c *dockerInspect, revision string) provider.Instance {
	name := strings.TrimPrefix(c.Name, "/")
	st := c.State
	inst := provider.Instance{
		Name:         name,
		Revision:     revision,
		Created:      parseDockerTime(c.Created),
		RestartCount: c.RestartCount,
		CrashLooping: st.Restarting,
	}

	running := st.Running && !st.Restarting && !st.Paused
	switch {
	case !running:
		inst.NotReadySince = firstNonZero(parseDockerTime(st.FinishedAt), inst.Created)
	case st.Health == nil:
		inst.Ready = true
	case st.Health.Status == "healthy":
		inst.Ready = true
	case st.Health.Status == "unhealthy":
		inst.NotReadySince = firstNonZero(failingSince(st.Health.Log), parseDockerTime(st.StartedAt), inst.Created)
	default: // "starting": not ready since the container started
		inst.NotReadySince = firstNonZero(parseDockerTime(st.StartedAt), inst.Created)
	}

	if finished := parseDockerTime(st.FinishedAt); !finished.IsZero() {
		inst.Terminations = []provider.Termination{dockerTermination(name, finished, st.ExitCode, st.OOMKilled)}
	}
	return inst
}

// failingSince returns the start of the trailing run of failed probes
// in the healthcheck log. Docker keeps only the last few probes, so for
// a streak longer than the log this is the oldest failure still on
// record — a lower bound on how long the container has been unhealthy.
func failingSince(log []healthProbe) time.Time {
	var since time.Time
	for i := len(log) - 1; i >= 0 && log[i].ExitCode != 0; i-- {
		since = parseDockerTime(log[i].Start)
	}
	return since
}

func dockerTermination(container string, at time.Time, exitCode int, oom bool) provider.Termination {
	reason := "Error"
	switch {
	case oom:
		reason = "OOMKilled"
	case exitCode == 0:
		reason = "Completed"
	case exitCode == 137: // SIGKILL: docker kill, stop timeout, or an OOM Docker did not attest
		reason = "Killed"
	}
	return provider.Termination{Container: container, Time: at, Reason: reason, ExitCode: exitCode, OOM: oom}
}

// parseDockerTime parses the RFC 3339 timestamps of docker inspect.
// Docker reports "0001-01-01T00:00:00Z" for events that never happened
// (a container that never stopped has that FinishedAt); it parses to
// the zero time, as does anything malformed.
func parseDockerTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t
}

func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
