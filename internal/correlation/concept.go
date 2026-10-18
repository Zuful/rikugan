// Package correlation hosts the higher-level synthesis tools that
// span multiple concepts — runes' "Six Eyes" pod diagnosis that
// reads kubernetes state and prometheus metrics in one shot.
//
// By design this concept does NOT have a provider abstraction:
// correlation tools are consumers of other concepts, not pluggable
// backends. Today runes calls the k8s client and promv1.API directly;
// a follow-up refactor (tracked in REFACTOR_PLAN.md §5) will route it
// through OrchestrationProvider and MetricsProvider so it stays
// agnostic of which backend the operator configured.
//
// Sub-packages: runes (the six_eyes correlator), deployhealth
// (check_deploy_health) and imageprovenance (get_image_provenance,
// which reads deployment state and answers in versioning terms). They
// are available only in the unified rikugan binary; the per-concept
// binaries deliberately don't include them because they cross concept
// boundaries.
package correlation
