// Package logs groups every provider that answers log-retrieval
// questions: fetch logs over a window, tail the most recent N lines,
// search by free-text expression or backend-native query.
//
// This package hosts the ConceptProvider that registers the canonical
// MCP tools — get_logs, tail_logs, search_logs — and routes each call
// through provider.LogsProvider to whichever backend the operator
// configured.
//
// Sub-package today: loki (implements LogsProvider, also exposes the
// detect_log_anomalies analytics tool outside the concept interface).
// kubectl-logs and docker-logs backends are tracked for follow-up
// PRs — they will plug into the same ConceptProvider without changing
// the tool surface.
package logs
