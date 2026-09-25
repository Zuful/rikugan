// Package tracing groups every provider that exposes distributed
// tracing data: find slow traces for a service, analyse a single
// trace's span tree.
//
// Sub-package today: jaeger. The concept does not yet have a formal
// Provider interface in pkg/provider — interface design waits until a
// second implementation (e.g. Tempo, OpenTelemetry collector) makes
// the abstraction honest. Tools are registered through the base
// Provider (Name + Tools) contract under their backend-specific
// names; they will move under a concept ConceptProvider when the
// interface lands.
package tracing
