// Package orchestration groups every provider that answers
// orchestration-shaped questions: what services exist, what state are
// they in, what events have they emitted, restart them, replay their
// history.
//
// Sub-packages: kubernetes (live cluster), and — once Étape 3 lands —
// dockercompose. Both implement provider.OrchestrationProvider so the
// concept-level MCP tools (list_services, get_service_state, …) can
// route to whichever backend the operator configured.
//
// Étape 2 status: sub-package code has been moved here under its
// original Provider (Name + Tools) shape. The concept-level interface
// in pkg/provider is declared but not yet implemented; concept-level
// tools are not yet exposed. See REFACTOR_PLAN.md.
package orchestration
