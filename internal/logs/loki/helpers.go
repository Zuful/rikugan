package loki

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Shared loki-package constants. The concept-level Logs provider
// (internal/logs) has its own copies of these for its own request
// validation — these constants are only the loki-side defaults used by
// LogsProvider methods and the anomalies tool.
const (
	opTimeout     = 30 * time.Second
	defaultLimit  = 100
	maxLimit      = 500
	maxLogLineLen = 200
)

// logEntry is the loki-package shape: a single line with its stream
// labels formatted as a single key=value comma-joined string. It is
// used by the anomalies tool, which predates the concept-level
// provider.LogEntry shape.
type logEntry struct {
	Timestamp time.Time
	Labels    string
	Line      string
}

// buildLabelQuery constructs a LogQL stream selector for a service
// + namespace combination using the app + namespace label convention.
func buildLabelQuery(service, namespace string) string {
	var parts []string
	if service != "" {
		parts = append(parts, fmt.Sprintf(`app=%q`, service))
	}
	if namespace != "" {
		parts = append(parts, fmt.Sprintf(`namespace=%q`, namespace))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// flattenStreams turns Loki's grouped-by-stream response into a flat
// list of log entries sorted newest-first.
func flattenStreams(streams []stream) []logEntry {
	var entries []logEntry
	for _, s := range streams {
		labels := formatLabels(s.Stream)
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			entries = append(entries, logEntry{
				Timestamp: parseNanoTimestamp(v[0]),
				Labels:    labels,
				Line:      v[1],
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Timestamp.After(entries[j].Timestamp)
	})
	return entries
}

// formatLabels renders a stream's label set as a stable, sorted
// `k=v,k=v` string suitable for table cells.
func formatLabels(labels map[string]string) string {
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

// parseNanoTimestamp converts a Loki nanosecond-epoch string into a
// time.Time. Returns the zero time on parse error — the anomalies
// tool tolerates that since timestamps are only used for ordering.
func parseNanoTimestamp(ns string) time.Time {
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n)
}
