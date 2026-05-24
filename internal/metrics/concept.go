// Package metrics groups every provider that answers metric-retrieval
// questions: fetch a series, compare to a baseline, list available
// metric names.
//
// Sub-package: prometheus (today). kubectl-top is TODO for a follow-up
// PR after the refactor lands. All sub-packages will implement
// provider.MetricsProvider so concept-level tools (get_metric,
// compare_to_baseline, list_available_metrics) route via operator
// config.
//
// Étape 2 status: see REFACTOR_PLAN.md — code moved, interface
// declared, providers not yet implementing it.
package metrics
