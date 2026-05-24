// Package tracing groups every provider that exposes distributed
// tracing data: find slow traces for a service, analyse a single
// trace's span tree.
//
// Sub-package: jaeger. The concept doesn't yet have a formal Provider
// interface in pkg/provider — interface design waits until a second
// implementation (e.g. Tempo, OpenTelemetry collector) makes the
// abstraction honest.
//
// Étape 2 status: see REFACTOR_PLAN.md — code moved, interface
// deferred until a second implementation arrives.
package tracing
