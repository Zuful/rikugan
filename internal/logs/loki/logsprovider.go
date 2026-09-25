package loki

import (
	"context"
	"fmt"
	"strings"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.LogsProvider —
// the concept-level Logs tools (get_logs / tail_logs / search_logs) can
// route to this backend.
var _ provider.LogsProvider = (*Provider)(nil)

// GetLogs returns log lines for the time window described by q. Q.Match
// is appended to the LogQL stream selector as a |= "<match>" filter
// when non-empty.
func (p *Provider) GetLogs(ctx context.Context, q provider.LogQuery) ([]provider.LogEntry, error) {
	logql := buildLogQL(q)
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	streams, err := p.client.QueryRange(ctx, logql, ToNano(q.Start), ToNano(q.End), limit)
	if err != nil {
		return nil, fmt.Errorf("loki query: %w", err)
	}
	return toLogEntries(streams), nil
}

// TailLogs returns the most recent N lines that match q's selector. The
// time window is taken from q (or defaults to the last hour if q.Start
// and q.End are both zero).
func (p *Provider) TailLogs(ctx context.Context, q provider.LogQuery, lines int) ([]provider.LogEntry, error) {
	tail := q
	if tail.Limit <= 0 || tail.Limit > lines {
		tail.Limit = lines
	}
	return p.GetLogs(ctx, tail)
}

// SearchLogs is GetLogs with the contract that q.Match (or a raw LogQL
// expression in q.Match) is mandatory; otherwise the call is rejected
// to avoid returning the world.
func (p *Provider) SearchLogs(ctx context.Context, q provider.LogQuery) ([]provider.LogEntry, error) {
	if strings.TrimSpace(q.Match) == "" && len(q.Labels) == 0 && q.Service == "" {
		return nil, fmt.Errorf("loki SearchLogs requires Match, Labels, or Service to scope the query")
	}
	return p.GetLogs(ctx, q)
}

// buildLogQL constructs a LogQL query string from a LogQuery. If
// q.Match looks like a fully-formed LogQL expression (starts with `{`)
// it is used as-is; otherwise a stream selector is built from the
// labels and q.Match is appended as a substring filter.
func buildLogQL(q provider.LogQuery) string {
	if strings.HasPrefix(strings.TrimSpace(q.Match), "{") {
		return q.Match
	}
	selector := buildLabelQueryFromQuery(q)
	if q.Match == "" {
		return selector
	}
	return fmt.Sprintf(`%s |= %q`, selector, q.Match)
}

func buildLabelQueryFromQuery(q provider.LogQuery) string {
	var parts []string
	if q.Service != "" {
		parts = append(parts, fmt.Sprintf(`app=%q`, q.Service))
	}
	if q.Namespace != "" {
		parts = append(parts, fmt.Sprintf(`namespace=%q`, q.Namespace))
	}
	for k, v := range q.Labels {
		parts = append(parts, fmt.Sprintf(`%s=%q`, k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// toLogEntries converts Loki streams into the concept-level shape.
func toLogEntries(streams []stream) []provider.LogEntry {
	var out []provider.LogEntry
	for _, s := range streams {
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			out = append(out, provider.LogEntry{
				Time:    parseNanoTimestamp(v[0]),
				Labels:  cloneLabels(s.Stream),
				Message: v[1],
			})
		}
	}
	return out
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
