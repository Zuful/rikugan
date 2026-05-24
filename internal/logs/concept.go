// Package logs groups every provider that answers log-retrieval
// questions: search logs across services, tail them, fetch a window
// around a timestamp.
//
// Sub-package: loki (today). kubectl-logs and docker-logs are TODO
// for follow-up PRs after the refactor lands. All sub-packages will
// implement provider.LogsProvider so concept-level tools (get_logs,
// tail_logs, search_logs) route based on operator config.
//
// Étape 2 status: see REFACTOR_PLAN.md — code moved, interface
// declared, providers not yet implementing it.
package logs
