package logs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	opTimeout    = 30 * time.Second
	defaultLimit = 100
	maxLimit     = 500
	maxLineLen   = 500
)

// Compile-time check that *ConceptProvider satisfies provider.Provider
// so the dispatcher can register it like any other provider.
var _ provider.Provider = (*ConceptProvider)(nil)

// ConceptProvider is the concept-level tool registry for the logs
// concept. It wraps the operator-configured provider.LogsProvider
// implementation (loki today, kubectl/docker logs once they exist) and
// exposes a single, technology-agnostic set of tools — get_logs,
// tail_logs, search_logs — that route through the interface and
// format the result as Markdown for MCP clients.
//
// The underlying provider stays registered separately when it has
// extra non-canonical tools (e.g. loki's detect_log_anomalies).
type ConceptProvider struct {
	impl     provider.LogsProvider
	implName string // human label of the backend, surfaced in reports
	logger   *slog.Logger
}

// NewConceptProvider wires the concept tools to a specific LogsProvider
// implementation. implName is the backend's short label (e.g. "loki")
// used in report headers and log fields.
func NewConceptProvider(impl provider.LogsProvider, implName string, logger *slog.Logger) *ConceptProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConceptProvider{
		impl:     impl,
		implName: implName,
		logger:   logger.With(slog.String("provider", "logs"), slog.String("backend", implName)),
	}
}

// Name returns the provider identifier surfaced to the dispatcher.
func (c *ConceptProvider) Name() string { return "logs" }

// Tools returns the canonical Logs concept tools.
func (c *ConceptProvider) Tools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		c.getLogsTool(),
		c.tailLogsTool(),
		c.searchLogsTool(),
	}
}

// ── Tool definitions ────────────────────────────────────────────────────────

func (c *ConceptProvider) getLogsTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_logs",
		mcp.WithDescription(
			"Fetch log lines for a service over a time window, via the configured "+
				"logs backend (loki, kubectl, docker-logs …). At least one of `service`, "+
				"`namespace`, or `query` must be supplied to scope the call. "+
				"[Methodology: Errors signal of LEST — see docs/methodology.md]",
		),
		mcp.WithString("service", mcp.Description("Service name (mapped to the backend's primary label, e.g. `app` for Loki).")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace to scope the query.")),
		mcp.WithString("query", mcp.Description("Optional native query expression: a full LogQL selector for Loki, overriding service/namespace selector building. Other backends have no query language: there it is a plain substring the lines must contain (e.g. `error`, not `status >= 500`).")),
		mcp.WithString("start", mcp.Description("Start time in RFC3339 (default: 1 hour ago).")),
		mcp.WithString("end", mcp.Description("End time in RFC3339 (default: now).")),
		mcp.WithNumber("limit", mcp.Description("Max log lines to return (default 100, max 500).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleGetLogs}
}

func (c *ConceptProvider) tailLogsTool() provider.ToolDefinition {
	tool := mcp.NewTool("tail_logs",
		mcp.WithDescription(
			"Fetch the most recent N log lines for a service via the configured "+
				"logs backend. Convenience wrapper over get_logs with limit semantics. "+
				"[Methodology: Errors signal of LEST — see docs/methodology.md]",
		),
		mcp.WithString("service", mcp.Description("Service name (mapped to the backend's primary label).")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace to scope the query.")),
		mcp.WithString("query", mcp.Description("Optional native query expression overriding service/namespace (a LogQL selector for Loki); a plain substring of the lines for other backends.")),
		mcp.WithNumber("lines", mcp.Description("Number of recent lines to fetch (default 100, max 500).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleTailLogs}
}

func (c *ConceptProvider) searchLogsTool() provider.ToolDefinition {
	tool := mcp.NewTool("search_logs",
		mcp.WithDescription(
			"Search logs matching a free-text expression or a native backend query, "+
				"scoped by service / namespace / labels. Useful for finding specific "+
				"error messages or patterns across services. "+
				"[Methodology: Errors signal of LEST (and Traffic when querying access-log volume) — see docs/methodology.md]",
		),
		mcp.WithString("match", mcp.Description("Substring or backend-native expression to match (e.g. a LogQL selector for Loki, or a substring for any backend).")),
		mcp.WithString("service", mcp.Description("Service name to scope the search.")),
		mcp.WithString("namespace", mcp.Description("Kubernetes namespace to scope the search.")),
		mcp.WithString("start", mcp.Description("Start time in RFC3339 (default: 1 hour ago).")),
		mcp.WithString("end", mcp.Description("End time in RFC3339 (default: now).")),
		mcp.WithNumber("limit", mcp.Description("Max matches to return (default 100, max 500).")),
	)
	return provider.ToolDefinition{Tool: tool, Handler: c.handleSearchLogs}
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (c *ConceptProvider) handleGetLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	q := buildQueryFromRequest(req)
	entries, err := c.impl.GetLogs(ctx, q)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("logs backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildLogsReport("Logs", c.implName, q, entries)), nil
}

func (c *ConceptProvider) handleTailLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	q := buildQueryFromRequest(req)
	if q.Start.IsZero() {
		q.Start = time.Now().Add(-1 * time.Hour)
		q.End = time.Now()
	}
	lines := defaultLimit
	if v, err := req.RequireFloat("lines"); err == nil && v > 0 {
		lines = int(v)
	}
	if lines > maxLimit {
		lines = maxLimit
	}
	entries, err := c.impl.TailLogs(ctx, q, lines)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("logs backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildLogsReport("Tail Logs", c.implName, q, entries)), nil
}

func (c *ConceptProvider) handleSearchLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	q := buildQueryFromRequest(req)
	match, _ := req.RequireString("match")
	q.Match = match
	entries, err := c.impl.SearchLogs(ctx, q)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("logs backend %q error: %v", c.implName, err)), nil
	}
	return mcp.NewToolResultText(buildLogsReport("Search Logs", c.implName, q, entries)), nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func buildQueryFromRequest(req mcp.CallToolRequest) provider.LogQuery {
	q := provider.LogQuery{
		Service:   req.GetString("service", ""),
		Namespace: req.GetString("namespace", ""),
		Match:     req.GetString("query", ""),
	}
	if v, err := req.RequireFloat("limit"); err == nil && v > 0 {
		q.Limit = int(v)
	}
	now := time.Now()
	q.Start = now.Add(-1 * time.Hour)
	q.End = now
	if v, _ := req.RequireString("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Start = t
		}
	}
	if v, _ := req.RequireString("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.End = t
		}
	}
	return q
}

func buildLogsReport(title, backend string, q provider.LogQuery, entries []provider.LogEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", title)
	fmt.Fprintf(&b, "**Backend:** `%s`  \n", backend)
	if q.Service != "" {
		fmt.Fprintf(&b, "**Service:** `%s`  \n", q.Service)
	}
	if q.Namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`  \n", q.Namespace)
	}
	if q.Match != "" {
		fmt.Fprintf(&b, "**Query/Match:** `%s`  \n", q.Match)
	}
	fmt.Fprintf(&b, "**Range:** %s → %s  \n", q.Start.UTC().Format(time.RFC3339), q.End.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "**Lines returned:** %d\n\n", len(entries))

	if len(entries) == 0 {
		b.WriteString("_No log lines found._\n")
		return b.String()
	}

	// Newest first.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Time.After(entries[j].Time)
	})

	b.WriteString("| Timestamp | Stream | Log Line |\n")
	b.WriteString("|-----------|--------|----------|\n")
	for _, e := range entries {
		msg := e.Message
		if len(msg) > maxLineLen {
			msg = msg[:maxLineLen-3] + "..."
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n",
			e.Time.UTC().Format(time.RFC3339),
			compactLabels(e.Labels),
			escapeMarkdown(msg),
		)
	}
	return b.String()
}

func compactLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, ", ")
}

// escapeMarkdown prevents pipe characters in log content from breaking
// the surrounding table rendering.
func escapeMarkdown(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
