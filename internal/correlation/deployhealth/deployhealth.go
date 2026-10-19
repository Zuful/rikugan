// Package deployhealth exposes check_deploy_health: did the service's
// last rollout break something? It is a correlation tool — a consumer of
// the deployments, orchestration and metrics concepts through their
// interfaces — and all the rules live in internal/health.
package deployhealth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

const opTimeout = 60 * time.Second

// Provider registers check_deploy_health.
type Provider struct {
	collector *health.Collector
	logger    *slog.Logger
}

// New builds the tool provider on a collector.
func New(collector *health.Collector, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{collector: collector, logger: logger}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "correlation/deploy-health" }

// Tools implements provider.Provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	tool := mcp.NewTool("check_deploy_health",
		mcp.WithDescription(
			"Did the service's last rollout break something? Compares the minutes after the rollout "+
				"completed (at the +5, +15 or +30 min checkpoint) with the 30 minutes before it started: "+
				"latency p95, 5xx rate, restarts and OOM kills of the new revision, readiness, CPU/memory "+
				"per replica, traffic. Returns ok / investigate / degraded / insufficient_data with the "+
				"numbers behind each signal. A rollout still in progress or completed under 5 minutes ago "+
				"gets no verdict but its new revision's restarts and readiness are reported; a stalled "+
				"rollout is degraded; one completed over 2 hours ago gets no verdict. "+
				"[Methodology: Step 0 — what changed? and LEST, all four signals — see docs/methodology.md]",
		),
		mcp.WithString("name", mcp.Required(), mcp.Description("Service name: the Deployment on Kubernetes, the service on Docker Compose.")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Kubernetes namespace, or Docker Compose project.")),
	)
	return []provider.ToolDefinition{{Tool: tool, Handler: p.handle}}
}

func (p *Provider) handle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	namespace, err := req.RequireString("namespace")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	svc := provider.Service{Name: name, Namespace: namespace, Kind: "deployment"}

	report, err := p.collector.Check(ctx, svc)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(Render(report)), nil
}

// Render formats a report as Markdown for the calling agent.
func Render(r health.Report) string {
	var b strings.Builder
	ro := r.Rollout
	fmt.Fprintf(&b, "## Deploy health — `%s/%s`\n\n", ro.Service.Namespace, ro.Service.Name)
	fmt.Fprintf(&b, "**Rollout:** revision %s (`%s`)", orDash(ro.Label()), orDash(ro.Revision))
	if ro.Rollback {
		b.WriteString(", rollback")
	}
	if len(ro.Images) > 0 {
		fmt.Fprintf(&b, ", images `%s`", strings.Join(ro.Images, "`, `"))
	}
	b.WriteString("  \n")
	fmt.Fprintf(&b, "**Started:** %s", stamp(ro.StartedAt))
	switch ro.State {
	case provider.RolloutComplete:
		fmt.Fprintf(&b, " · **Completed:** %s", stamp(ro.CompletedAt))
	case provider.RolloutFailed:
		fmt.Fprintf(&b, " · **Failed:** %s", stamp(ro.FailedAt))
	default:
		b.WriteString(" · **In progress**")
	}
	b.WriteString("\n\n")

	switch r.Outcome {
	case health.OutcomeEvaluated:
		if r.Checkpoint > 0 {
			fmt.Fprintf(&b, "### Verdict: **%s** (checkpoint +%s)\n\n", strings.ToUpper(string(r.Verdict)), r.Checkpoint)
		} else {
			fmt.Fprintf(&b, "### Verdict: **%s**\n\n", strings.ToUpper(string(r.Verdict)))
		}
	case health.OutcomeInProgress:
		b.WriteString("### No verdict: rollout in progress\n\n")
	case health.OutcomeTooEarly:
		fmt.Fprintf(&b, "### No verdict yet: first check possible at %s\n\n", stamp(r.NextCheck))
	case health.OutcomeStale:
		b.WriteString("### No verdict: rollout too old for a before/after comparison\n\n")
		b.WriteString("Use `compare_to_baseline` for the service's current state.\n\n")
	}

	if len(r.Signals) > 0 {
		b.WriteString("| Signal | Status | Detail |\n|---|---|---|\n")
		for _, s := range r.Signals {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", s.Name, s.Status, strings.ReplaceAll(s.Reason, "|", "\\|"))
		}
		b.WriteString("\n")
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
