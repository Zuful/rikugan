// Package metrics groups every provider that answers metric-retrieval
// questions: fetch a series over a time window, compare current values
// to a historical baseline, list available metric names for discovery.
//
// This package hosts the ConceptProvider that registers the canonical
// MCP tools — get_metric, compare_to_baseline, list_available_metrics
// — and routes each call through provider.MetricsProvider to whichever
// backend the operator configured.
//
// Sub-package today: prometheus (implements MetricsProvider; no
// remaining backend-specific tools — every operation projects into
// the concept interface). A kubectl-top backend is tracked for a
// follow-up PR and will plug into the same ConceptProvider without
// changing the tool surface.
package metrics
