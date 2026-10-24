package dockercompose

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// The Provider is also the logs concept's backend when no log store is
// configured: docker logs reads what the daemon kept of each
// container's output. It covers the containers that still exist, so a
// container removed by a redeploy takes its logs with it.
var _ provider.LogsProvider = (*Provider)(nil)

const (
	defaultLogLimit = 100
	// maxLogRead bounds what one container's docker logs may return
	// before filtering: the most recent lines of the window.
	maxLogRead = 5000
)

// CombinedRunner is a CommandRunner that can also return a command's
// stdout and stderr together: docker logs replays a container's two
// streams onto its own, and services log on either.
type CombinedRunner interface {
	RunCombined(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (execRunner) RunCombined(ctx context.Context, name string, args ...string) ([]byte, error) {
	// #nosec G204 -- as in Run: name is the literal "docker", no shell,
	// and the one external value, a container ID from docker compose ps,
	// follows "--".
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %v: %w (output: %s)", name, args, err, truncateOutput(out))
	}
	return out, nil
}

func truncateOutput(b []byte) string {
	const limit = 300
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

// GetLogs returns the lines the service's containers wrote between
// q.Start and q.End, newest first, at most q.Limit. Without a service,
// every container of the project. q.Match, when set, keeps the lines
// containing it, case aside: Compose has no query language.
func (p *Provider) GetLogs(ctx context.Context, q provider.LogQuery) ([]provider.LogEntry, error) {
	return p.logs(ctx, q, maxLogRead)
}

// TailLogs returns the last lines of the service's containers.
func (p *Provider) TailLogs(ctx context.Context, q provider.LogQuery, lines int) ([]provider.LogEntry, error) {
	if lines <= 0 {
		lines = defaultLogLimit
	}
	q.Limit = lines
	return p.logs(ctx, q, lines)
}

// SearchLogs is GetLogs with q.Match: the lines containing it.
func (p *Provider) SearchLogs(ctx context.Context, q provider.LogQuery) ([]provider.LogEntry, error) {
	if strings.TrimSpace(q.Match) == "" {
		return nil, fmt.Errorf("search needs a match: a substring of the lines to find")
	}
	return p.logs(ctx, q, maxLogRead)
}

func (p *Provider) logs(ctx context.Context, q provider.LogQuery, tail int) ([]provider.LogEntry, error) {
	if err := validateComposeName("service", q.Service); err != nil {
		return nil, err
	}
	all, err := p.dockerComposePS(ctx, q.Namespace)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	match := strings.ToLower(q.Match)
	var entries []provider.LogEntry
	for _, c := range all {
		if q.Service != "" && c.Service != q.Service {
			continue
		}
		args := []string{"logs", "--timestamps", "--tail", strconv.Itoa(tail)}
		if !q.Start.IsZero() {
			args = append(args, "--since", q.Start.UTC().Format(time.RFC3339))
		}
		if !q.End.IsZero() {
			args = append(args, "--until", q.End.UTC().Format(time.RFC3339))
		}
		out, err := p.runCombined(ctx, append(args, "--", c.ID)...)
		if err != nil {
			return nil, fmt.Errorf("docker logs %s: %w", c.Name, err)
		}
		labels := map[string]string{"service": c.Service, "container": c.Name}
		for _, e := range parseDockerLogs(out, labels) {
			if match == "" || strings.Contains(strings.ToLower(e.Message), match) {
				entries = append(entries, e)
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.After(entries[j].Time) })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func (p *Provider) runCombined(ctx context.Context, args ...string) ([]byte, error) {
	if r, ok := p.runner.(CombinedRunner); ok {
		return r.RunCombined(ctx, "docker", args...)
	}
	return p.runner.Run(ctx, "docker", args...)
}

// parseDockerLogs reads docker logs --timestamps output: each line
// starts with its RFC 3339 time. A line without one (a multi-line
// message cut by the daemon) is kept, timed like the line before.
func parseDockerLogs(raw []byte, labels map[string]string) []provider.LogEntry {
	var out []provider.LogEntry
	var last time.Time
	for _, line := range bytes.Split(raw, []byte("\n")) {
		s := strings.TrimRight(string(line), "\r")
		if strings.TrimSpace(s) == "" {
			continue
		}
		msg := s
		if ts, rest, ok := strings.Cut(s, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				last, msg = t, rest
			}
		}
		out = append(out, provider.LogEntry{Time: last, Labels: labels, Message: msg})
	}
	return out
}
