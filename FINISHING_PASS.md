# Rikugan — Finishing pass post-refactor concept-first

Three targeted gaps from the original brief that the refactor PR did not
close. This document is the étape-A plan; nothing in the codebase is
touched until the user validates.

---

## Audit findings

| # | Claim from the brief | Verified |
| --- | --- | --- |
| 1 | `ORCHESTRATION_PROVIDER` env var does not exist in code | `grep -rn ORCHESTRATION_PROVIDER --include='*.go'` returns only a forward-looking doc comment in `internal/orchestration/dockercompose/dockercompose.go` plus two prose mentions in `REFACTOR_PLAN.md` and `MIGRATION.md`. No `os.Getenv("ORCHESTRATION_PROVIDER")` anywhere. **Confirmed.** |
| 2 | Docker Compose `OrchestrationProvider` is not wired at runtime | `internal/app/concepts.go::registerOrchestrationConcept` registers only `orchk8s.NewFromClient(k8sClient, …)` and the concept provider wrapping it. The `dockercompose` package is imported by no production caller. **Confirmed.** |
| 3 | Dispatcher rejects providers with `len(Tools()) == 0` | `pkg/dispatcher/dispatcher.go:32` — `if len(tools) == 0 { return fmt.Errorf("provider %q exposes no tools", p.Name()) }`. `dockercompose.Provider.Tools()` returns `nil` — registering it would fail. **Confirmed latent bug.** |
| 4 | README in hybrid state | Sections 38-380 still describe the per-technology Seven Eyes model; étape-3+ concept sections were inserted at lines 391-414 but the document is internally inconsistent. **Confirmed.** |
| 5 | Doc references stale "to come" language | `MIGRATION.md` final section says "What étape 4 will add (preview, not delivered yet)" listing per-concept binaries and routing env vars — both are now live. Several package comments mention "Étape 2 status" or "future env var" for things already in code. **Confirmed.** |
| 6 | `docs/methodology.md` references removed tool names | Pre-refactor names like `pagerduty.list_active_incidents`, `pagerduty.list_service_incidents` still appear; the concept tools that replaced them aren't mentioned. **Confirmed.** |

---

## Plan — three commits in order, with hard stops between them

### Étape B — runtime wiring of Docker Compose + smoke test  ➝  one commit

1. **`pkg/dispatcher/dispatcher.go`** — relax `Register` so a provider
   with `Tools() == nil` is accepted. Append it to `d.providers` and
   log at INFO that it's registered without direct tools (it exposes
   its operations via a concept interface). Update
   `pkg/dispatcher/dispatcher_test.go` so the existing
   `TestRegister_EmptyProvider` flips from "expects error" to
   "accepts the registration, asserts the provider list grew",
   and add one positive assertion for the no-tools branch.

2. **`internal/app/concepts.go::registerOrchestrationConcept`** —
   honour `ORCHESTRATION_PROVIDER` (`kubernetes` default,
   `dockercompose` supported; unknown values warn + fall back to
   kubernetes, mirroring `ALERTS_PROVIDER`). On `dockercompose`:
   build via `dockercompose.New(WithLogger(logger),
   WithProject(os.Getenv("COMPOSE_PROJECT_NAME")))`, register the
   provider (zero tools, allowed now), then register
   `orchestration.NewConceptProvider(...)` wrapping it.

   **k8sClient nil-safety — least-invasive choice:** keep the
   `k8sClient kubernetes.Interface` parameter; nil it out only at
   the *caller* in `SetupOrchestration` when env=dockercompose
   (where k8s is irrelevant for this binary). `Setup()` (unified)
   keeps building the k8s client unconditionally — the unified
   binary's contract is "everything is wired", so dockercompose
   stays a per-concept-binary feature in this pass. A short
   comment in `registerOrchestrationConcept` documents the
   rationale and an `if k8sClient == nil && backend == kubernetes`
   guard returns a clear error in case the caller forgets.

3. **`internal/orchestration/abstraction_test.go`** (new) — smoke
   test proving the concept abstraction routes through both
   backends. Uses `k8s.io/client-go/kubernetes/fake` (pattern
   already in use in `internal/correlation/runes/tool_six_eyes_test.go`)
   plus the existing `stubRunner` from
   `internal/orchestration/dockercompose/orchestrationprovider_test.go`.
   For each backend: wrap in `ConceptProvider`, call
   `ListServices(ctx, "")` through the `OrchestrationProvider`
   interface, assert non-empty results from both.

4. Standard sweep: `go build ./...`, `go test -race ./...`.

**Manual smoke command** for the user to run end-to-end after
the commit lands:

```bash
export ORCHESTRATION_PROVIDER=dockercompose
export COMPOSE_PROJECT_NAME=mystack
./bin/rikugan-orchestration  # then list_services via JSON-RPC
```

### Étape C — README rewrite around the concept-first model  ➝  one commit

Re-shape README in the order the brief specifies (title, MCP intro,
Architecture, per-concept binaries, Tools-by-concept, Getting Started
with `ORCHESTRATION_PROVIDER`/`ALERTS_PROVIDER`, migration pointer,
methodology + contributing + license unchanged). Reuse every existing
description (tools, philosophy paragraphs) — reorganise and reframe,
do not rewrite from scratch. Drop the "Seven Eyes" framing as primary
value prop; the table that currently lists "techno → tools" flips to
"concept → backends → tools".

### Étape D — doc-drift sync  ➝  one commit

Pure doc edits, no code touched:

1. Package comments under `internal/{orchestration,logs,metrics,alerts}/concept.go` (and any subordinate
   files still saying "Étape N to come" for shipped work).
2. `MIGRATION.md` — replace "What étape 4 will add (preview)" with
   "Étape 4 — delivered" describing actual binaries + env vars.
3. `docs/methodology.md` — rename references:
   `pagerduty.list_active_incidents` → `list_active_alerts` (concept,
   `ALERTS_PROVIDER=pagerduty`); `pagerduty.list_service_incidents`
   → `list_active_alerts` with `service` argument; keep
   `pagerduty.get_current_oncalls` (backend-specific tool, pointer
   to `MIGRATION.md`).
4. `REFACTOR_PLAN.md` — add a one-paragraph header
   *"This plan has been fully executed. See MIGRATION.md and
   README.md for current state."* and leave the body untouched.
5. `go build ./...` + `go test ./...` to confirm nothing broke.

---

## Out of scope (caught during audit, not fixing here)

- `runes` (correlation) imports the k8s client and `promv1.API`
  directly instead of routing through `OrchestrationProvider` /
  `MetricsProvider` as REFACTOR_PLAN §5 envisioned. Refactor that
  later — the brief explicitly says no new scope.
- `pkg/restclient/` helper was planned in étape 2 but never extracted;
  each HTTP-based provider still has its own `doJSON` copy. Not in
  scope here.
- `cmd/rikugan-mcp/` target tree name (REFACTOR_PLAN §3) was never
  applied — the unified binary stayed `cmd/rikugan/`. Either is fine;
  no rename in this pass.

---

**Stopping here.** Awaiting validation before touching code.
