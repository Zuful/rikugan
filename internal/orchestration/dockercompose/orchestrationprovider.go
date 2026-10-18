package dockercompose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// ── Docker Compose JSON model (minimal projection) ──────────────────────────

// composePS is the shape of one line in `docker compose ps --format json`.
// Compose 2.x emits one JSON object per line (NDJSON).
type composePS struct {
	ID         string `json:"ID"`
	Name       string `json:"Name"`
	Image      string `json:"Image"`
	Project    string `json:"Project"`
	Service    string `json:"Service"`
	State      string `json:"State"`  // running, exited, paused, restarting, dead, created
	Status     string `json:"Status"` // human description, e.g. "Up 5 minutes (healthy)"
	Health     string `json:"Health"` // healthy, unhealthy, starting, "" when no healthcheck
	ExitCode   int    `json:"ExitCode"`
	Created    int64  `json:"Created"` // unix epoch
	Command    string `json:"Command"`
	Publishers []struct {
		PublishedPort int    `json:"PublishedPort"`
		TargetPort    int    `json:"TargetPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

// dockerInspect is the subset of `docker inspect <container>` used to
// describe a service (image, restart count, env, mounts). The full
// response is much richer; we keep just what's needed for the
// markdown report.
// healthProbe is one entry of a container's healthcheck log, oldest
// first. Docker keeps only the last few.
type healthProbe struct {
	Start    string `json:"Start"`
	ExitCode int    `json:"ExitCode"`
}

type dockerInspect struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Image   string `json:"Image"`
	State   struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status        string        `json:"Status"`
			FailingStreak int           `json:"FailingStreak"`
			Log           []healthProbe `json:"Log"`
		} `json:"Health,omitempty"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Config       struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

// ── Method implementations ──────────────────────────────────────────────────

// ListServices returns every service declared in the Compose project.
// `namespace` is interpreted as the Compose project name; when empty
// the provider's default project (or Compose's auto-detected one) is
// used. Each running container becomes one Service entry — services
// with no running container are not emitted, matching how
// `docker compose ps` behaves by default.
func (p *Provider) ListServices(ctx context.Context, namespace string) ([]provider.Service, error) {
	containers, err := p.dockerComposePS(ctx, namespace)
	if err != nil {
		return nil, err
	}
	// Collapse multiple replicas of the same service into one Service.
	seen := map[string]bool{}
	out := make([]provider.Service, 0, len(containers))
	for _, c := range containers {
		key := c.Project + "/" + c.Service
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, provider.Service{
			Name:      c.Service,
			Namespace: c.Project,
			Kind:      kindDockerComposeService,
			Labels: map[string]string{
				"image": c.Image,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetServiceState reports container state for the service. Compose
// services may have N replicas (scale > 1); the state aggregates them:
// status=running iff every replica is running and healthy (when
// healthchecks are defined); status=failed if any is exited; degraded
// otherwise. Desired replicas is best-guessed from the count of
// containers Compose returns for the service — Compose doesn't expose
// a "desired" count separate from "what's defined in the YAML".
func (p *Provider) GetServiceState(ctx context.Context, svc provider.Service) (provider.ServiceState, error) {
	containers, err := p.containersForService(ctx, svc)
	if err != nil {
		return provider.ServiceState{}, err
	}
	state := provider.ServiceState{}
	state.Replicas.Desired = len(containers)
	if len(containers) == 0 {
		state.Status = "unknown"
		state.Message = "no containers found for service — has it been started?"
		return state, nil
	}
	var failed, running int
	var firstUnhealthy string
	for _, c := range containers {
		switch c.State {
		case "running":
			running++
		case "exited", "dead":
			failed++
		}
		if c.Health == "unhealthy" && firstUnhealthy == "" {
			firstUnhealthy = c.Name
		}
	}
	state.Replicas.Available = running
	switch {
	case failed > 0:
		state.Status = "failed"
		state.Message = fmt.Sprintf("%d of %d replica(s) exited", failed, len(containers))
	case running == len(containers) && firstUnhealthy == "":
		state.Status = "running"
	case running == len(containers) && firstUnhealthy != "":
		state.Status = "degraded"
		state.Message = fmt.Sprintf("container %s reports unhealthy", firstUnhealthy)
	default:
		state.Status = "degraded"
		state.Message = fmt.Sprintf("%d/%d replicas running", running, len(containers))
	}
	return state, nil
}

// DescribeService renders a Markdown description of the service's
// containers (image, state, healthcheck, restart policy, labels).
// It walks every container belonging to the service and concatenates
// their key fields.
func (p *Provider) DescribeService(ctx context.Context, svc provider.Service) (string, error) {
	containers, err := p.containersForService(ctx, svc)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Service `%s` (Docker Compose project `%s`)\n\n", svc.Name, p.resolveProject(svc.Namespace))
	if len(containers) == 0 {
		b.WriteString("_No containers running for this service._\n")
		return b.String(), nil
	}
	for i, c := range containers {
		insp, err := p.dockerInspect(ctx, c.ID)
		if err != nil {
			fmt.Fprintf(&b, "\n### Container %d — %s (inspect failed: %v)\n", i+1, c.Name, err)
			continue
		}
		fmt.Fprintf(&b, "\n### Container %d — `%s`\n\n", i+1, c.Name)
		fmt.Fprintf(&b, "| Field | Value |\n|-------|-------|\n")
		fmt.Fprintf(&b, "| Image | `%s` |\n", insp.Config.Image)
		fmt.Fprintf(&b, "| State | %s |\n", insp.State.Status)
		fmt.Fprintf(&b, "| Restart policy | %s |\n", insp.HostConfig.RestartPolicy.Name)
		fmt.Fprintf(&b, "| Restart count | %d |\n", insp.RestartCount)
		fmt.Fprintf(&b, "| Created | %s |\n", insp.Created)
		fmt.Fprintf(&b, "| Started | %s |\n", insp.State.StartedAt)
		if insp.State.Health != nil {
			fmt.Fprintf(&b, "| Healthcheck | %s (failing streak: %d) |\n", insp.State.Health.Status, insp.State.Health.FailingStreak)
		}
		if len(insp.Config.Labels) > 0 {
			b.WriteString("\n**Labels:**\n\n```json\n")
			raw, _ := json.MarshalIndent(insp.Config.Labels, "", "  ")
			b.Write(raw)
			b.WriteString("\n```\n")
		}
	}
	return b.String(), nil
}

// GetRecentEvents returns Docker daemon events filtered to the
// service's containers within the lookback window. Implemented via
// `docker events --since <since> --until now --filter container=<name>`
// for each container, then merged.
//
// Note: `docker events` is a streaming command but with both --since
// and --until set, it returns immediately. Each line is one JSON
// event.
func (p *Provider) GetRecentEvents(ctx context.Context, svc provider.Service, since time.Duration) ([]provider.Event, error) {
	if since <= 0 {
		since = time.Hour
	}
	containers, err := p.containersForService(ctx, svc)
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, nil
	}

	var events []provider.Event
	for _, c := range containers {
		args := []string{
			"events",
			"--since", fmt.Sprintf("%ds", int(since.Seconds())),
			"--until", "0s",
			"--format", "{{json .}}",
			"--filter", "container=" + c.Name,
		}
		out, err := p.runner.Run(ctx, "docker", args...)
		if err != nil {
			// Per-container event fetch is best-effort; log and continue.
			p.logger.Warn("docker events failed", "container", c.Name, "error", err.Error())
			continue
		}
		events = append(events, parseDockerEvents(out)...)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.After(events[j].Time) })
	return events, nil
}

// RestartService runs `docker compose restart <service>` against the
// project. Matches `kubectl rollout restart deployment` semantics:
// containers are stopped and re-created.
func (p *Provider) RestartService(ctx context.Context, svc provider.Service) error {
	project := p.resolveProject(svc.Namespace)
	if err := validateComposeName("project", project); err != nil {
		return err
	}
	if err := validateComposeName("service", svc.Name); err != nil {
		return err
	}
	args := []string{"compose", "-p", project, "restart", svc.Name}
	if _, err := p.runner.Run(ctx, "docker", args...); err != nil {
		return fmt.Errorf("docker compose restart: %w", err)
	}
	return nil
}

// GetServiceHistory is the **soft tear** in the OrchestrationProvider
// abstraction when applied to Docker Compose. Compose has no native
// rolling-update history concept: there is no equivalent of a
// Kubernetes ReplicaSet, no recorded sequence of past image tags.
//
// Rather than return an error and break the interface, this method
// returns a Markdown report that explains the limitation and suggests
// alternatives the operator can pursue manually (image registry tags,
// git log of the compose file, CI build logs). The concept tool
// still works against this backend — it just degrades to a documented
// dead-end. The decision to keep the method in the interface (rather
// than narrow the interface and promote it to a kubernetes-specific
// tool) is recorded in MIGRATION.md and REFACTOR_PLAN.md: a future
// backend (nomad, ECS, swarm) can either implement it or follow the
// same fallback pattern.
func (p *Provider) GetServiceHistory(ctx context.Context, svc provider.Service) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## Service History — `%s` (Docker Compose)\n\n", svc.Name)
	b.WriteString("_Docker Compose does not record rolling-update history natively._ ")
	b.WriteString("Unlike Kubernetes ReplicaSets, Compose does not preserve a sequence ")
	b.WriteString("of past pod-template versions; redeploys overwrite the live state.\n\n")
	b.WriteString("**Alternatives to recover the equivalent information manually:**\n\n")
	b.WriteString("- The container image registry's tag history (`docker image ls` + registry browser)\n")
	b.WriteString("- The Compose file's revisions in git (`git log -- docker-compose.yml`)\n")
	b.WriteString("- CI/CD build logs (Argo CD, GitHub Actions, GitLab CI)\n\n")

	// Best-effort: show the current image tag(s) for the service so the
	// caller at least knows what's deployed *right now*.
	containers, err := p.containersForService(ctx, svc)
	if err == nil && len(containers) > 0 {
		b.WriteString("**Currently deployed:**\n\n")
		b.WriteString("| Container | Image |\n|-----------|-------|\n")
		for _, c := range containers {
			fmt.Fprintf(&b, "| `%s` | `%s` |\n", c.Name, c.Image)
		}
	}
	return b.String(), nil
}

// ── Internal helpers ────────────────────────────────────────────────────────

// composeNamePattern is the shape Docker Compose itself accepts for project
// and service names. The rule that matters for safety is the first
// character: a name like "-v" would reach docker as a flag rather than as a
// value, which is the argument-injection shape gosec (G204) and Bearer
// (CWE-78) flag on execRunner.Run.
var composeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// validateComposeName rejects a project or service name that docker could
// parse as a flag. An empty name means "not supplied" and is left to the
// caller's own defaulting, so it passes.
func validateComposeName(kind, name string) error {
	if name == "" {
		return nil
	}
	if !composeNamePattern.MatchString(name) {
		return fmt.Errorf("invalid %s name %q: expected letters, digits, dot, dash or underscore, and not starting with a dash", kind, name)
	}
	return nil
}

func (p *Provider) resolveProject(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return p.project
}

// dockerComposePS calls `docker compose -p <project> ps --format json -a`
// and decodes the NDJSON output.
func (p *Provider) dockerComposePS(ctx context.Context, project string) ([]composePS, error) {
	args := []string{"compose"}
	if proj := p.resolveProject(project); proj != "" {
		if err := validateComposeName("project", proj); err != nil {
			return nil, err
		}
		args = append(args, "-p", proj)
	}
	args = append(args, "ps", "-a", "--format", "json")
	out, err := p.runner.Run(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	return parseComposePS(out), nil
}

func (p *Provider) containersForService(ctx context.Context, svc provider.Service) ([]composePS, error) {
	all, err := p.dockerComposePS(ctx, svc.Namespace)
	if err != nil {
		return nil, err
	}
	out := make([]composePS, 0)
	for _, c := range all {
		if c.Service == svc.Name {
			out = append(out, c)
		}
	}
	return out, nil
}

func (p *Provider) dockerInspect(ctx context.Context, id string) (dockerInspect, error) {
	out, err := p.runner.Run(ctx, "docker", "inspect", id)
	if err != nil {
		return dockerInspect{}, fmt.Errorf("docker inspect: %w", err)
	}
	// `docker inspect` returns a JSON array of one element.
	var arr []dockerInspect
	if err := json.Unmarshal(out, &arr); err != nil {
		return dockerInspect{}, fmt.Errorf("decode inspect: %w", err)
	}
	if len(arr) == 0 {
		return dockerInspect{}, fmt.Errorf("docker inspect returned empty array")
	}
	return arr[0], nil
}

// parseComposePS accepts either NDJSON (one object per line, Compose 2.x
// default) or a single JSON array. Both forms are seen in the wild
// depending on the Compose version.
func parseComposePS(raw []byte) []composePS {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var arr []composePS
		_ = json.Unmarshal(raw, &arr)
		return arr
	}
	var out []composePS
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var c composePS
		if err := json.Unmarshal(line, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// parseDockerEvents decodes the NDJSON stream from `docker events
// --format {{json .}}` into provider.Event values. Unknown fields are
// preserved in Message for the operator to read.
type dockerEvent struct {
	Type   string `json:"Type"`   // container, image, network, ...
	Action string `json:"Action"` // start, die, kill, ...
	Time   int64  `json:"time"`   // unix epoch seconds
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func parseDockerEvents(raw []byte) []provider.Event {
	var out []provider.Event
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e dockerEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, provider.Event{
			Time:    time.Unix(e.Time, 0),
			Type:    e.Type,
			Reason:  e.Action,
			Message: e.Actor.Attributes["name"] + " " + e.Action,
		})
	}
	return out
}
