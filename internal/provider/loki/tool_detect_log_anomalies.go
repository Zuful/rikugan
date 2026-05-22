package loki

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const (
	anomalyLimit     = 1000
	maxErrorPatterns = 10
	maxSampleLines   = 5
)

// detectLogAnomaliesTool returns the ToolDefinition for detect_log_anomalies.
func (p *Provider) detectLogAnomaliesTool() provider.ToolDefinition {
	tool := mcp.NewTool("detect_log_anomalies",
		mcp.WithDescription(
			"Analyze recent logs for a service and detect anomalies. "+
				"Classifies log lines by level, counts error/warning rates, "+
				"and groups error messages by normalized pattern. "+
				"Useful for spotting recurring issues or sudden error spikes. "+
				"[Methodology: Errors signal of LEST — see docs/methodology.md]",
		),
		mcp.WithString("service",
			mcp.Required(),
			mcp.Description("Service name (used as the app label in LogQL)"),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace to scope the query"),
		),
		mcp.WithString("duration",
			mcp.Description("Lookback duration (e.g. \"1h\", \"30m\"). Default: 1h"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleDetectLogAnomalies,
	}
}

// handleDetectLogAnomalies is the MCP handler for detect_log_anomalies.
func (p *Provider) handleDetectLogAnomalies(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	service, err := req.RequireString("service")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	namespace, _ := req.RequireString("namespace")

	duration := 1 * time.Hour
	if d, _ := req.RequireString("duration"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			duration = parsed
		}
	}

	log := p.logger.With(slog.String("tool", "detect_log_anomalies"))
	log.Info("tool called",
		"service", service,
		"namespace", namespace,
		"duration", duration.String(),
	)

	now := time.Now()
	query := buildLabelQuery(service, namespace)

	streams, err := p.client.QueryRange(ctx, query, ToNano(now.Add(-duration)), ToNano(now), anomalyLimit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("loki API error: %v", err)), nil
	}

	entries := flattenStreams(streams)

	report := buildAnomalyReport(service, namespace, duration, entries)
	return mcp.NewToolResultText(report), nil
}

// ── Log level classification ────────────────────────────────────────────────

type logLevel string

const (
	levelError logLevel = "ERROR"
	levelWarn  logLevel = "WARN"
	levelInfo  logLevel = "INFO"
	levelDebug logLevel = "DEBUG"
	levelOther logLevel = "OTHER"
)

// classifyLevel scans a log line for common level tokens.
func classifyLevel(line string) logLevel {
	upper := strings.ToUpper(line)
	switch {
	case strings.Contains(upper, "ERROR") || strings.Contains(upper, "FATAL") || strings.Contains(upper, "PANIC"):
		return levelError
	case strings.Contains(upper, "WARN"):
		return levelWarn
	case strings.Contains(upper, "INFO"):
		return levelInfo
	case strings.Contains(upper, "DEBUG") || strings.Contains(upper, "TRACE"):
		return levelDebug
	default:
		return levelOther
	}
}

// ── Pattern normalization ───────────────────────────────────────────────────

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[^\s]*`)
	reUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reNumbers   = regexp.MustCompile(`\b\d+\b`)
)

// normalizePattern strips timestamps, UUIDs, and numbers from a log line
// to produce a grouping key.
func normalizePattern(line string) string {
	s := reTimestamp.ReplaceAllString(line, "<TS>")
	s = reUUID.ReplaceAllString(s, "<UUID>")
	s = reNumbers.ReplaceAllString(s, "<N>")
	if len(s) > maxLogLineLen {
		s = s[:maxLogLineLen-3] + "..."
	}
	return s
}

// ── Error pattern grouping ──────────────────────────────────────────────────

type errorPattern struct {
	Pattern   string
	Count     int
	FirstSeen time.Time
	LastSeen  time.Time
}

func groupErrorPatterns(entries []logEntry) []errorPattern {
	m := map[string]*errorPattern{}
	for _, e := range entries {
		if classifyLevel(e.Line) != levelError {
			continue
		}
		pat := normalizePattern(e.Line)
		g, ok := m[pat]
		if !ok {
			g = &errorPattern{
				Pattern:   pat,
				FirstSeen: e.Timestamp,
				LastSeen:  e.Timestamp,
			}
			m[pat] = g
		}
		g.Count++
		if e.Timestamp.Before(g.FirstSeen) {
			g.FirstSeen = e.Timestamp
		}
		if e.Timestamp.After(g.LastSeen) {
			g.LastSeen = e.Timestamp
		}
	}

	patterns := make([]errorPattern, 0, len(m))
	for _, g := range m {
		patterns = append(patterns, *g)
	}

	sort.Slice(patterns, func(i, j int) bool {
		return patterns[i].Count > patterns[j].Count
	})

	if len(patterns) > maxErrorPatterns {
		patterns = patterns[:maxErrorPatterns]
	}

	return patterns
}

// ── Report builder ──────────────────────────────────────────────────────────

func buildAnomalyReport(service, namespace string, duration time.Duration, entries []logEntry) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Log Anomaly Analysis\n\n")
	fmt.Fprintf(&b, "**Service:** `%s`  \n", service)
	if namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`  \n", namespace)
	}
	fmt.Fprintf(&b, "**Duration:** %s  \n", duration.String())
	fmt.Fprintf(&b, "**Lines analyzed:** %d\n\n", len(entries))

	if len(entries) == 0 {
		b.WriteString("_No log lines found for this service._\n")
		return b.String()
	}

	// Level distribution.
	counts := map[logLevel]int{}
	for _, e := range entries {
		counts[classifyLevel(e.Line)]++
	}

	total := len(entries)
	b.WriteString("### Level Distribution\n\n")
	b.WriteString("| Level | Count | % of Total |\n")
	b.WriteString("|-------|-------|------------|\n")

	for _, lvl := range []logLevel{levelError, levelWarn, levelInfo, levelDebug, levelOther} {
		c := counts[lvl]
		if c == 0 {
			continue
		}
		pct := float64(c) / float64(total) * 100
		fmt.Fprintf(&b, "| %s | %d | %.1f%% |\n", lvl, c, pct)
	}
	b.WriteString("\n")

	// Summary sentence.
	errorCount := counts[levelError]
	warnCount := counts[levelWarn]
	fmt.Fprintf(&b, "**Summary:** %d error(s) and %d warning(s) out of %d total lines.\n\n", errorCount, warnCount, total)

	// Error patterns.
	patterns := groupErrorPatterns(entries)
	if len(patterns) > 0 {
		b.WriteString("### Top Error Patterns\n\n")
		b.WriteString("| Count | Pattern | First Seen | Last Seen |\n")
		b.WriteString("|-------|---------|------------|----------|\n")

		for _, p := range patterns {
			fmt.Fprintf(&b, "| %d | %s | %s | %s |\n",
				p.Count,
				p.Pattern,
				p.FirstSeen.UTC().Format(time.RFC3339),
				p.LastSeen.UTC().Format(time.RFC3339),
			)
		}
		b.WriteString("\n")
	}

	// Sample error lines.
	var errorLines []logEntry
	for _, e := range entries {
		if classifyLevel(e.Line) == levelError {
			errorLines = append(errorLines, e)
			if len(errorLines) >= maxSampleLines {
				break
			}
		}
	}
	if len(errorLines) > 0 {
		b.WriteString("### Sample Error Lines\n\n")
		for _, e := range errorLines {
			fmt.Fprintf(&b, "- `%s` — %s\n", e.Timestamp.UTC().Format(time.RFC3339), e.Line)
		}
		b.WriteString("\n")
	}

	// Note at limit.
	if len(entries) == anomalyLimit {
		fmt.Fprintf(&b, "_Analysis based on %d most recent lines. Older entries may exist._\n", anomalyLimit)
	}

	return b.String()
}
