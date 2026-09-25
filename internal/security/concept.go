// Package security groups every provider that exposes runtime
// security signals: rule firings, audit event analysis.
//
// Sub-package today: falco. The concept does not yet have a formal
// Provider interface in pkg/provider — interface design waits until
// a second implementation (e.g. Tracee, Tetragon) makes the
// abstraction honest. Tools are registered through the base Provider
// (Name + Tools) contract under their backend-specific names; they
// will move under a concept ConceptProvider when the interface lands.
package security
