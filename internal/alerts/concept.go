// Package alerts groups every provider that surfaces actionable
// runtime signals — AlertManager firings, Sentry application
// exceptions, PagerDuty operational incidents. They sit under one
// concept because the agent-facing operations are the same shape
// (list active, get details, acknowledge, resolve, silence) even
// though the underlying domains differ.
//
// Sub-packages: alertmanager (new in Étape 2), sentry, pagerduty.
// All will implement provider.AlertsProvider in Étape 3; the risk of
// interface deformation is tracked in REFACTOR_PLAN.md, with the
// fallback of splitting Sentry out into its own Errors concept if the
// AlertsProvider can't honestly express what each backend does.
//
// Provider-specific tools that cannot project into the AlertsProvider
// interface (e.g. PagerDuty's on-call rotations) stay outside the
// interface as concept-level optional tools.
//
// Étape 2 status: see REFACTOR_PLAN.md — code moved, AlertManager
// scaffold added, interface declared, providers not yet implementing
// it.
package alerts
