// Package alerts groups every provider that surfaces actionable
// runtime signals — AlertManager firings, Sentry application
// exceptions, PagerDuty operational incidents. They sit under one
// concept because the agent-facing read operations are the same
// shape (list active alerts, get an alert's details) even though
// the underlying domains differ.
//
// This package hosts the ConceptProvider that registers the canonical
// MCP tools — list_active_alerts, get_alert_details — and routes
// them through the operator-selected backend. The ALERTS_PROVIDER
// env var picks the primary (alertmanager / sentry / pagerduty);
// non-selected backends still register their backend-specific
// extras (sentry.list_issues_for_release, pagerduty.get_current_oncalls).
//
// AlertsProvider is the strict intersection of read operations across
// the three backends. Mutating operations (ack / resolve / silence)
// don't intersect cleanly — AlertManager has no per-alert ack, Sentry
// resolves but doesn't ack, PagerDuty has all three with different
// semantics — and stay as backend-specific concerns outside the
// interface. See MIGRATION.md for the tool mapping from the
// pre-refactor backend-prefixed names.
package alerts
