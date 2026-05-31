// Package orchestration groups every provider that answers
// orchestration-shaped questions: what services exist, what state are
// they in, what events have they emitted, restart them, replay their
// history.
//
// This package hosts the ConceptProvider that registers the canonical
// MCP tools — list_services, get_service_state, describe_service,
// get_recent_events, restart_service, get_service_history — and
// routes each call through provider.OrchestrationProvider to whichever
// backend the operator configured. ORCHESTRATION_PROVIDER selects the
// backend (default kubernetes, also dockercompose).
//
// Sub-packages:
//   - kubernetes: live cluster backend, also exposes the
//     find_pods_by_service_name discovery helper outside the
//     concept interface.
//   - dockercompose: implements the same OrchestrationProvider
//     contract via the docker CLI. Validates that the interface
//     generalises beyond Kubernetes (étape-3 abstraction gate);
//     GetServiceHistory degrades gracefully there since Compose has
//     no native rolling-update history.
package orchestration
