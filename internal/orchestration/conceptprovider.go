package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	opTimeout       = 30 * time.Second
	defaultLookback = 1 * time.Hour
)

// Compile-time check that *ConceptProvider satisfies provider.Provider.
var _ provider.Provider = (*ConceptProvider)(nil)

// ConceptProvider is the concept-level tool registry for the
// orchestration concept. It wraps the operator-selected
// provider.OrchestrationProvider implementation (kubernetes today,
// docker-compose in chunk E) and exposes six technology-agnostic
// tools — list_services, get_service_state, describe_service,
// get_recent_events, restart_service, get_service_history — that route
// through the OrchestrationProvider interface and format results as
// Markdown.
type ConceptProvider struct {
	impl     provider.OrchestrationProvider
	implName string
	logger   *slog.Logger
}

// NewConceptProvider wires the concept tools to a specific
// OrchestrationProvider implementation.
func NewConceptProvider(impl provider.OrchestrationProvider, implName string, logger *slog.Logger) *ConceptProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConceptProvider{
		impl:     impl,
		implName: implName,
		logger:   logger.With(slog.String("provider", "orchestration"), slog.String("backend", implName)),
	}
}

// Name returns the provider identifier surfaced to the dispatcher.
func (c *ConceptProvider) Name() string { return "orchestration" }

// Tools returns the canonical Orchestration concept tools.
func (c *ConceptProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		c.listServicesTool(),
		c.getServiceStateTool(),
		c.describeServiceTool(),
		c.getRecentEventsTool(),
		c.restartServiceTool(),
		c.getServiceHistoryTool(),
	}
}

// ── Tool definitions ────────────────────────────────────────────────────────

func (c *ConceptProvider) listServicesTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_services",
		mcp.WithDescription(
			"List services managed by the configured orchestration backend "+
				"(kubernetes today, docker-compose in development). For kubernetes "+
				"a `service` is a Deployment. Pass an optional namespace/project "+
				"to scope the listing. "+
				"[Methodology: Step 0 blast-radius scoping — see docs/methodology.md]",
		),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace / Docker Compose project. Empty = all.")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleListServices}
}

func (c *ConceptProvider) getServiceStateTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_service_state",
		mcp.WithDescription(
			"Return the current health snapshot for a service: status "+
				"(running/degraded/failed/pending/unknown), desired vs available "+
				"replica counts, and a human message. "+
				"[Methodology: Errors and Saturation of LEST — see docs/methodology.md]",
		),
		mcp.WithString("name", mcp.Required(), mcp.Description("Service name (Deployment name in kubernetes; service name in docker-compose).")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace / Docker Compose project.")),
		mcp.WithString("kind", mcp.Description("Service kind. For kubernetes: deployment (default) or pod. For docker-compose: docker-compose-service.")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleGetServiceState}
}

func (c *ConceptProvider) describeServiceTool() provider.ToolDefinition {
	tool := mcp.NewTool("describe_service",
		mcp.WithDescription(
			"Return a structured Markdown description of the service: spec "+
				"(image, replicas, strategy, labels) and current status. The "+
				"orchestration-flavoured equivalent of `kubectl describe` / "+
				"`docker compose inspect`.",
		),
		mcp.WithString("name", mcp.Required(), mcp.Description("Service name.")),
		mcp.WithString("namespace", mcp.Description("Namespace / project.")),
		mcp.WithString("kind", mcp.Description("Service kind (default: deployment for kubernetes).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleDescribeService}
}

func (c *ConceptProvider) getRecentEventsTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_recent_events",
		mcp.WithDescription(
			"Return noteworthy orchestration events attached to the service "+
				"in the lookback window (restarts, scheduling failures, OOMKills, "+
				"image pull errors). For kubernetes this is the events API "+
				"filtered to the service's pods. "+
				"[Methodology: Errors signal of LEST — see docs/methodology.md]",
		),
		mcp.WithString("name", mcp.Required(), mcp.Description("Service name.")),
		mcp.WithString("namespace", mcp.Description("Namespace / project.")),
		mcp.WithString("kind", mcp.Description("Service kind (default: deployment).")),
		mcp.WithString("since", mcp.Description("Lookback duration (e.g. \"15m\", \"1h\", \"24h\"). Default 1h.")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleGetRecentEvents}
}

func (c *ConceptProvider) restartServiceTool() provider.ToolDefinition {
	tool := mcp.NewTool("restart_service",
		mcp.WithDescription(
			"Trigger a rolling restart of the service. For kubernetes a "+
				"deployment is patched with the standard restartedAt annotation "+
				"(same mechanism as `kubectl rollout restart`); for a pod the pod "+
				"is deleted. **Mutating** — make sure the operator intends this.",
		),
		mcp.WithString("name", mcp.Required(), mcp.Description("Service name.")),
		mcp.WithString("namespace", mcp.Description("Namespace / project.")),
		mcp.WithString("kind", mcp.Description("Service kind (default: deployment).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleRestartService}
}

func (c *ConceptProvider) getServiceHistoryTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_service_history",
		mcp.WithDescription(
			"Return the rolling-update history for the service. For kubernetes "+
				"this lists every ReplicaSet the deployment has owned, sorted by "+
				"revision; for docker-compose this is best-effort and may not be "+
				"supported. "+
				"[Methodology: Step 0 \"what changed\" — see docs/methodology.md]",
		),
		mcp.WithString("name", mcp.Required(), mcp.Description("Service name.")),
		mcp.WithString("namespace", mcp.Description("Namespace / project.")),
		mcp.WithString("kind", mcp.Description("Service kind (default: deployment).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleGetServiceHistory}
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *ConceptProvider) handleListServices(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	ns := req.GetString("namespace", "")
	services, err := c.impl.ListServices(ctx, ns)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("orchestration backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildListServicesReport(c.implName, ns, services)), nil
}

func (c *ConceptProvider) handleGetServiceState(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	svc, err := serviceFromRequest(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	state, err := c.impl.GetServiceState(ctx, svc)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("orchestration backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildStateReport(c.implName, svc, state)), nil
}

func (c *ConceptProvider) handleDescribeService(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	svc, err := serviceFromRequest(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	report, err := c.impl.DescribeService(ctx, svc)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("orchestration backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(report), nil
}

func (c *ConceptProvider) handleGetRecentEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	svc, err := serviceFromRequest(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	since := defaultLookback
	if v := req.GetString("since", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			since = d
		}
	}
	events, err := c.impl.GetRecentEvents(ctx, svc, since)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("orchestration backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildEventsReport(c.implName, svc, since, events)), nil
}

func (c *ConceptProvider) handleRestartService(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	svc, err := serviceFromRequest(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := c.impl.RestartService(ctx, svc); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("orchestration backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		"## Service Restart Triggered\n\n**Backend:** `%s`  \n**Service:** `%s/%s` (kind=%s)\n\nThe restart has been issued; use `get_service_state` shortly to confirm replicas converge.\n",
		c.implName, svc.Namespace, svc.Name, fallback(svc.Kind, "deployment"),
	)), nil
}

func (c *ConceptProvider) handleGetServiceHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	svc, err := serviceFromRequest(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	report, err := c.impl.GetServiceHistory(ctx, svc)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("orchestration backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(report), nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func serviceFromRequest(req mcp.CallToolRequest) (provider.Service, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return provider.Service{}, err
	}
	return provider.Service{
		Name:      name,
		Namespace: req.GetString("namespace", ""),
		Kind:      req.GetString("kind", ""),
	}, nil
}

func buildListServicesReport(backend, namespace string, services []provider.Service) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Services\n\n")
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	if namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`  \n", namespace)
	} else {
		b.WriteString("**Namespace:** _all_  \n")
	}
	fmt.Fprintf(&b, "**Services returned:** %d\n\n", len(services))

	if len(services) == 0 {
		b.WriteString("_No services found._\n")
		return b.String()
	}
	sort.Slice(services, func(i, j int) bool {
		if services[i].Namespace != services[j].Namespace {
			return services[i].Namespace < services[j].Namespace
		}
		return services[i].Name < services[j].Name
	})
	b.WriteString("| Name | Namespace | Kind | Labels |\n|------|-----------|------|--------|\n")
	for _, s := range services {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n",
			s.Name, s.Namespace, fallback(s.Kind, "—"), summarizeLabels(s.Labels))
	}
	return b.String()
}

func buildStateReport(backend string, svc provider.Service, state provider.ServiceState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Service State — %s/%s\n\n", svc.Namespace, svc.Name)
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	fmt.Fprintf(&b, "**Kind:** `%s`\n\n", fallback(svc.Kind, "deployment"))
	fmt.Fprintf(&b, "| Field | Value |\n|-------|-------|\n")
	fmt.Fprintf(&b, "| Status | %s |\n", state.Status)
	fmt.Fprintf(&b, "| Replicas | %d / %d (available / desired) |\n", state.Replicas.Available, state.Replicas.Desired)
	if state.Message != "" {
		fmt.Fprintf(&b, "| Message | %s |\n", state.Message)
	}
	return b.String()
}

func buildEventsReport(backend string, svc provider.Service, since time.Duration, events []provider.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Recent Events — %s/%s (last %s)\n\n", svc.Namespace, svc.Name, since)
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	fmt.Fprintf(&b, "**Events returned:** %d\n\n", len(events))
	if len(events) == 0 {
		b.WriteString("_No recent events for this service._\n")
		return b.String()
	}
	b.WriteString("| Time | Type | Reason | Message |\n|------|------|--------|---------|\n")
	for _, e := range events {
		msg := e.Message
		if len(msg) > 100 {
			msg = msg[:97] + "..."
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			e.Time.UTC().Format(time.RFC3339), e.Type, e.Reason, escapePipe(msg))
	}
	return b.String()
}

func summarizeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pick := keys
	truncated := false
	if len(pick) > 3 {
		pick = pick[:3]
		truncated = true
	}
	parts := make([]string, 0, len(pick))
	for _, k := range pick {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	out := strings.Join(parts, ", ")
	if truncated {
		out += ", …"
	}
	return out
}

func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func escapePipe(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// jsonOrEmpty marshals v as compact JSON; used only by tests/helpers
// that want to log the raw extras side-channel without dragging the
// orchestration interface into JSON formatting.
func jsonOrEmpty(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(out)
}

var _ = jsonOrEmpty
