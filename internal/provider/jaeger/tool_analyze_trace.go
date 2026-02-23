package jaeger

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const topSpanCount = 3

// analyzeTraceTool returns the ToolDefinition for analyze_trace.
func (p *JaegerProvider) analyzeTraceTool() provider.ToolDefinition {
	tool := mcp.NewTool("analyze_trace",
		mcp.WithDescription(
			"Analyze a Jaeger trace and return a synthesized bottleneck report. "+
				"Identifies the slowest spans and any spans with errors, without "+
				"returning raw JSON. Useful for pinpointing latency sources and "+
				"failures in distributed request paths.",
		),
		mcp.WithString("trace_id",
			mcp.Required(),
			mcp.Description("Jaeger trace ID to analyze"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleAnalyzeTrace,
	}
}

// handleAnalyzeTrace is the MCP handler for analyze_trace.
func (p *JaegerProvider) handleAnalyzeTrace(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	traceID, err := req.RequireString("trace_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	log := p.logger.With(slog.String("tool", "analyze_trace"))
	log.Info("tool called", "trace_id", traceID)

	trace, err := p.client.GetTrace(ctx, traceID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("jaeger API error: %v", err)), nil
	}

	report := buildTraceAnalysis(trace)
	return mcp.NewToolResultText(report), nil
}

// ── Analysis logic ──────────────────────────────────────────────────────────

type analyzedSpan struct {
	Service   string
	Operation string
	Duration  time.Duration
	HasError  bool
	ErrorMsg  string
}

func buildTraceAnalysis(trace *Trace) string {
	if len(trace.Spans) == 0 {
		return fmt.Sprintf("## Trace Analysis\n\n**Trace ID:** `%s`\n\n_No spans found._\n", trace.TraceID)
	}

	// Resolve spans with service names and error info.
	spans := make([]analyzedSpan, 0, len(trace.Spans))
	for _, s := range trace.Spans {
		svc := trace.Processes[s.ProcessID].ServiceName
		hasErr, errMsg := extractError(s)
		spans = append(spans, analyzedSpan{
			Service:   svc,
			Operation: s.OperationName,
			Duration:  time.Duration(s.Duration) * time.Microsecond,
			HasError:  hasErr,
			ErrorMsg:  errMsg,
		})
	}

	// Compute total trace duration from span boundaries.
	var minStart, maxEnd int64
	minStart = trace.Spans[0].StartTime
	for _, s := range trace.Spans {
		if s.StartTime < minStart {
			minStart = s.StartTime
		}
		end := s.StartTime + s.Duration
		if end > maxEnd {
			maxEnd = end
		}
	}
	totalDuration := time.Duration(maxEnd-minStart) * time.Microsecond
	startTime := time.UnixMicro(minStart).UTC().Format(time.RFC3339)

	// Sort by duration descending for bottleneck detection.
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].Duration > spans[j].Duration
	})

	// Collect error spans.
	var errorSpans []analyzedSpan
	for _, s := range spans {
		if s.HasError {
			errorSpans = append(errorSpans, s)
		}
	}

	// Top N slowest.
	top := spans
	if len(top) > topSpanCount {
		top = top[:topSpanCount]
	}

	// ── Build report ────────────────────────────────────────────────
	var b strings.Builder

	fmt.Fprintf(&b, "## Trace Analysis\n\n")
	fmt.Fprintf(&b, "**Trace ID:** `%s`  \n", trace.TraceID)
	fmt.Fprintf(&b, "**Start:** %s  \n", startTime)
	fmt.Fprintf(&b, "**Total duration:** %s  \n", formatDuration(totalDuration))
	fmt.Fprintf(&b, "**Total spans:** %d\n\n", len(trace.Spans))

	// Summary sentence.
	if len(top) > 0 {
		fmt.Fprintf(&b, "Trace `%s` took %s. The bottleneck was `%s` (%s) in service `%s`.",
			trace.TraceID,
			formatDuration(totalDuration),
			top[0].Operation,
			formatDuration(top[0].Duration),
			top[0].Service,
		)
		if len(errorSpans) > 0 {
			fmt.Fprintf(&b, " **%d span(s) reported errors.**", len(errorSpans))
		}
		b.WriteString("\n\n")
	}

	// Slowest spans table.
	fmt.Fprintf(&b, "### Slowest Spans\n\n")
	b.WriteString("| Rank | Service | Operation | Duration | %% of Total |\n")
	b.WriteString("|------|---------|-----------|----------|------------|\n")

	for i, s := range top {
		pct := float64(0)
		if totalDuration > 0 {
			pct = float64(s.Duration) / float64(totalDuration) * 100
		}
		fmt.Fprintf(&b, "| %d | `%s` | `%s` | %s | %.1f%% |\n",
			i+1, s.Service, s.Operation, formatDuration(s.Duration), pct,
		)
	}
	b.WriteString("\n")

	// Error spans.
	if len(errorSpans) > 0 {
		fmt.Fprintf(&b, "### Errors Found\n\n")
		b.WriteString("| Service | Operation | Duration | Error |\n")
		b.WriteString("|---------|-----------|----------|-------|\n")

		for _, s := range errorSpans {
			msg := s.ErrorMsg
			if len(msg) > 120 {
				msg = msg[:117] + "..."
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n",
				s.Service, s.Operation, formatDuration(s.Duration), msg,
			)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("_No errors found in this trace._\n\n")
	}

	return b.String()
}

// extractError checks a span's tags and logs for error indicators.
func extractError(s Span) (bool, string) {
	// Check tags for error=true.
	hasError := false
	var errMsg string
	for _, t := range s.Tags {
		if t.Key == "error" {
			if v, ok := t.Value.(bool); ok && v {
				hasError = true
			}
			// Some systems encode as string "true".
			if v, ok := t.Value.(string); ok && v == "true" {
				hasError = true
			}
		}
		if t.Key == "error.message" || t.Key == "otel.status_description" {
			if v, ok := t.Value.(string); ok {
				errMsg = v
			}
		}
	}

	// Check logs for error fields.
	if !hasError || errMsg == "" {
		for _, l := range s.Logs {
			for _, f := range l.Fields {
				if f.Key == "error" || f.Key == "error.object" || f.Key == "event" {
					if v, ok := f.Value.(string); ok {
						if f.Key == "event" && v != "error" {
							continue
						}
						hasError = true
						if errMsg == "" {
							errMsg = v
						}
					}
				}
				if f.Key == "message" && errMsg == "" {
					if v, ok := f.Value.(string); ok {
						errMsg = v
					}
				}
			}
		}
	}

	if hasError && errMsg == "" {
		errMsg = "error (no message)"
	}

	return hasError, errMsg
}
