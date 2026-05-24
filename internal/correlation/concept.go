// Package correlation hosts the higher-level synthesis tools that
// span multiple concepts — runes' "Six Eyes" pod diagnosis that
// reads kubernetes state and prometheus metrics in one shot.
//
// By design this concept does NOT have a provider abstraction:
// correlation tools are consumers of other concepts, not pluggable
// backends. After Étape 3 lands the OrchestrationProvider and
// MetricsProvider interfaces, runes will be refactored to call those
// instead of the k8s client and promv1.API directly, so it stays
// agnostic of which orchestration/metrics provider is in use.
//
// Étape 2 status: runes code moved here under its original package
// name; no behaviour change.
package correlation
