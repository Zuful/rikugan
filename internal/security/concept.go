// Package security groups every provider that exposes runtime
// security signals: rule firings, audit event analysis.
//
// Sub-package: falco. The concept doesn't yet have a formal Provider
// interface in pkg/provider — interface design waits until a second
// implementation makes the abstraction honest.
//
// Étape 2 status: see REFACTOR_PLAN.md — code moved, interface
// deferred until a second implementation arrives.
package security
