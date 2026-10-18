package dockercompose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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
// Terminations come from the daemon's event log (die and oom events
// since the container was created) because docker inspect cannot be
// trusted for them: when the restart policy starts a container again,
// Docker resets State.ExitCode to 0 and State.OOMKilled to false, so an
// OOM kill followed by a successful restart would read as a clean exit
// (observed against a real daemon in oom_integration_test.go). The
// event log is a bounded in-memory buffer, so on a busy daemon old
// events can be gone; see containerInstance for the fallback.
// RestartCount carries the full count either way.
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
		c := &inspected[i]
		var exits []provider.Termination
		if !parseDockerTime(c.State.FinishedAt).IsZero() {
			exits, err = p.containerExits(ctx, c.ID, strings.TrimPrefix(c.Name, "/"), parseDockerTime(c.Created))
			if err != nil {
				// Best effort: containerInstance falls back to inspect.
				p.logger.Warn("docker events failed", "container", c.Name, "error", err.Error())
			}
		}
		out = append(out, containerInstance(c, revision, exits))
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

// containerExits reads the container's die and oom events since it was
// created and turns each die into a termination, flagged OOM when an
// oom event came after the previous die (the kernel's OOM notification
// precedes the exit it causes).
func (p *Provider) containerExits(ctx context.Context, id, name string, since time.Time) ([]provider.Termination, error) {
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	out, err := p.runner.Run(ctx, "docker", "events",
		"--since", since.UTC().Format(time.RFC3339Nano),
		"--until", time.Now().UTC().Format(time.RFC3339Nano),
		"--filter", "container="+id,
		"--filter", "event=die",
		"--filter", "event=oom",
		"--format", "{{json .}}",
	)
	if err != nil {
		return nil, fmt.Errorf("docker events: %w", err)
	}
	return exitsFromEvents(out, name), nil
}

// exitsFromEvents turns each die event into a termination, flagged OOM
// when an oom event precedes it since the previous die. Against a real
// daemon the oom event was logged 70-235 ms before its die in every run
// where it was emitted, but in about one run in ten Docker emitted no
// oom event at all for a genuine OOM kill (oom_integration_test.go).
// That exit then reads as a plain SIGKILL — Killed, 137, OOM false —
// which understates it but never passes it off as a clean exit.
func exitsFromEvents(raw []byte, container string) []provider.Termination {
	var events []dockerEvent
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var e dockerEvent
		if json.Unmarshal(bytes.TrimSpace(line), &e) == nil && (e.Action == "die" || e.Action == "oom") {
			events = append(events, e)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return eventTime(events[i]).Before(eventTime(events[j])) })

	var exits []provider.Termination
	oom := false
	for _, e := range events {
		if e.Action == "oom" {
			oom = true
			continue
		}
		code, err := strconv.Atoi(e.Actor.Attributes["exitCode"])
		if err != nil {
			code = -1
		}
		exits = append(exits, dockerTermination(container, eventTime(e), code, oom))
		oom = false
	}
	return exits
}

func eventTime(e dockerEvent) time.Time {
	if e.TimeNano != 0 {
		return time.Unix(0, e.TimeNano).UTC()
	}
	return time.Unix(e.Time, 0).UTC()
}

// containerInstance builds the instance from docker inspect and the
// terminations read from the event log. When the log no longer has
// them, inspect is used only where it is still truthful: a container
// that is stopped or restarting still carries its last exit code and
// OOM flag, but one that is running again after a restart has had them
// reset, so its last stop is reported as Unknown rather than as the
// clean exit inspect would claim.
func containerInstance(c *dockerInspect, revision string, exits []provider.Termination) provider.Instance {
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

	finished := parseDockerTime(st.FinishedAt)
	switch {
	case len(exits) > 0:
		inst.Terminations = exits
	case finished.IsZero():
		// Never stopped.
	case running && c.RestartCount > 0:
		inst.Terminations = []provider.Termination{{Container: name, Time: finished, Reason: "Unknown", ExitCode: -1}}
	default:
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
