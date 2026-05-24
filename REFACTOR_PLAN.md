# Rikugan MCP — Refactor Plan: concept-first architecture

**Status :** Étape 1 — plan, awaiting validation. No code touched yet.
**Branch :** `refactor/concept-first`.

---

## 1. Objective

Today rikugan exposes its 24 MCP tools **by technology** — `query_range`
(Prometheus), `search_logs` (Loki), `diagnose_pod` (Kubernetes),
`get_security_events` (Falco), …  An agent calling rikugan has to know
which backend each tool is for.

We move to a **concept-first** model: tools are named by the operational
need they answer (`get_logs`, `get_metric`, `list_active_alerts`,
`restart_service`), with one or more interchangeable provider
implementations behind each concept. The agent stops caring whether the
logs come from Loki, kubectl, or Docker; the operator picks the
provider in config.

Final end-state is a monorepo with one binary per concept, sharing code
through `pkg/`. This plan splits that into four sequential stages with
a validation gate after each one.

---

## 2. Current inventory & concept mapping

24 tools spread across 11 provider packages today:

| Current package | Tool | Target concept | Target operation |
|---|---|---|---|
| `kubernetes` | `diagnose_pod` | Orchestration | `describe_service` (pod focus) |
| `kubernetes` | `find_pods_by_service_name` | Orchestration | `list_services` (with `service_name` filter) — helper that survives |
| `kubernetes` | `get_image_provenance` | Versioning (P1) | `get_image_provenance` |
| `prometheus` | `query_range` | Metrics | `get_metric` (PromQL passthrough remains; high-level helpers come later) |
| `loki` | `search_logs` | Logs | `search_logs` |
| `loki` | `get_log_context` | Logs | `get_logs` (window around a timestamp) |
| `loki` | `detect_log_anomalies` | Logs | `detect_anomalies` (analytics, kept as a logs-concept tool) |
| `jaeger` | `find_slow_traces` | Tracing | `find_slow_traces` |
| `jaeger` | `analyze_trace` | Tracing | `analyze_trace` |
| `falco` | `get_security_events` | Security | `list_security_events` |
| `falco` | `analyze_pod_audit` | Security | `analyze_pod_audit` |
| `sentry` | `list_recent_issues` | Alerts | `list_active_alerts` (Sentry provider) |
| `sentry` | `get_issue_detail` | Alerts | `get_alert_details` |
| `sentry` | `list_issues_for_release` | Alerts | `list_alerts_for_release` |
| `pagerduty` | `list_active_incidents` | Alerts | `list_active_alerts` (PagerDuty provider) |
| `pagerduty` | `list_service_incidents` | Alerts | `list_service_alerts` |
| `pagerduty` | `get_current_oncalls` | Alerts | `get_current_oncalls` (PD-specific, kept) |
| `argocd` | `get_app_status` | Versioning (P1) | `get_deployment_status` |
| `argocd` | `list_degraded_apps` | Versioning (P1) | `list_degraded_deployments` |
| `github` | `get_recent_commits` | Versioning (P1) | `get_recent_commits` |
| `github` | `get_commit_diff` | Versioning (P1) | `get_commit_diff` |
| `gitlab` | `gitlab_get_recent_commits` | Versioning (P1) | `get_recent_commits` (gitlab provider) |
| `gitlab` | `gitlab_get_commit_diff` | Versioning (P1) | `get_commit_diff` (gitlab provider) |
| `runes` | `six_eyes` | Correlation | `cross_signal_pod_diagnosis` |

Eight concepts total, plus Correlation as a special meta-concept.

### Concept catalogue (with current providers)

| Concept | Status | Providers (current) | Providers (target after refactor) |
|---|---|---|---|
| **Orchestration** | P0 | kubernetes | + docker-compose (new, validates the abstraction in Étape 3) |
| **Logs** | P0 | loki | + kubectl-logs (new), + docker-logs (new, *cf. TODO*) |
| **Metrics** | P0 | prometheus | + kubectl-top (new, *cf. TODO*) |
| **Alerts** | P0 | sentry, pagerduty | + alertmanager (new, written in Étape 2) |
| **Tracing** | non-prio | jaeger | — |
| **Security** | non-prio | falco | — |
| **Versioning** | P1 | github, gitlab, argocd | — |
| **Correlation** | special | runes | — (cross-concept consumer, no provider abstraction) |

### Tools that change name

Per the brief, every renamed tool gets a row in `MIGRATION.md`. Notable
renames (full list comes with Étape 3) :

- `query_range` → `get_metric`
- `search_logs` / `get_log_context` → `search_logs` / `get_logs`
- `get_security_events` → `list_security_events`
- `list_recent_issues` (Sentry) and `list_active_incidents` (PagerDuty)
  → both become `list_active_alerts` differentiated by config / provider
  routing.
- `gitlab_get_recent_commits` → `get_recent_commits` (provider-routed
  via config) — drops the `gitlab_` prefix that violated concept-first.
- `diagnose_pod` → `describe_service` (with `service_kind: pod`).
- `six_eyes` → `cross_signal_pod_diagnosis` (Correlation concept).

### Shared code targets (→ `pkg/`)

- `pkg/provider/` — `Provider` base interface (moves from
  `internal/provider/provider.go`) + per-concept interfaces
  (`OrchestrationProvider`, `LogsProvider`, …) introduced in Étape 3.
- `pkg/dispatcher/` — current `internal/dispatcher` (no behaviour
  change; renamed scope so per-concept binaries can use it).
- `pkg/mcp/` — small helpers around `mark3labs/mcp-go` that today live
  inline in `internal/app` (server construction, recovery, capability
  flags, registration loop).
- `pkg/logging/` — slog initialisation + the `parseLogLevel` helper
  currently in `cmd/rikugan/main.go`.
- `pkg/restclient/` — shared HTTP helpers : timeout-bound client,
  `doJSON` with status mapping, trailing-slash trimming. Each current
  client (`falco`, `jaeger`, `loki`, `pagerduty`, `sentry`, `argocd`)
  duplicates this pattern with small variations (auth header, error
  taxonomy). The shared helper provides hooks for both.

External SDK clients (`go-github`, `gitlab-org/api/client-go`, k8s
`client-go`, `prometheus/client_golang/api`) stay where they are —
they're not our code.

---

## 3. Target tree

```
rikugan/
├── cmd/
│   ├── rikugan-mcp/        # unified binary (back-compat, registers all concepts)
│   ├── orchestration-mcp/  # one binary per concept (introduced Étape 4)
│   ├── logs-mcp/
│   ├── metrics-mcp/
│   └── alerts-mcp/
├── internal/
│   ├── orchestration/
│   │   ├── concept.go      # MCP tool definitions for this concept
│   │   ├── kubernetes/     # k8s-backed OrchestrationProvider
│   │   └── dockercompose/  # added Étape 3 (validates the interface)
│   ├── logs/
│   │   ├── concept.go
│   │   ├── loki/
│   │   ├── kubectl/        # TODO Étape 2 (kubectl logs native)
│   │   └── docker/         # TODO (docker logs)
│   ├── metrics/
│   │   ├── concept.go
│   │   ├── prometheus/
│   │   └── kubectltop/     # TODO
│   ├── alerts/
│   │   ├── concept.go
│   │   ├── alertmanager/   # TODO Étape 2 (new provider, written from scratch)
│   │   ├── sentry/
│   │   └── pagerduty/
│   ├── tracing/
│   │   ├── concept.go
│   │   └── jaeger/
│   ├── security/
│   │   ├── concept.go
│   │   └── falco/
│   ├── versioning/         # P1, structure-only at Étape 2 then dormant
│   │   ├── concept.go
│   │   ├── github/
│   │   ├── gitlab/
│   │   └── argocd/
│   ├── correlation/        # special — no per-provider sub-packages
│   │   └── runes/          # six_eyes lives here
│   └── app/                # wiring stays internal (per-binary glue)
├── pkg/
│   ├── provider/           # base + per-concept interfaces
│   ├── dispatcher/
│   ├── mcp/                # MCP server helpers
│   ├── logging/            # slog init + parseLogLevel
│   └── restclient/         # HTTP helpers, doJSON, trailing-slash trim
├── scripts/
│   └── build-all.sh        # builds every cmd/* (Étape 4)
├── docs/
│   └── methodology.md      # existing LEST doc, untouched
├── REFACTOR_PLAN.md        # this file (deleted at the end?)
├── MIGRATION.md            # tool-rename table for consumers (Étape 3)
├── CHANGELOG.md            # cross-stage diary
├── Makefile
└── README.md               # rewrite touches Étape 4
```

---

## 4. Stage breakdown

### Étape 2 — Internal reorg, no external API change

Goal : same binary, same 24 tools exposed, code moved into the new tree.
A consumer (Claude Code, Cursor) running rikugan today shouldn't notice
anything.

1. Create `internal/<concept>/` skeleton for orchestration, logs,
   metrics, alerts, tracing, security, versioning, correlation. Each
   gets an empty `concept.go` placeholder.
2. Move the per-tech provider packages into the right concept folder
   *unchanged* :
   - `internal/provider/kubernetes/` → split into
     `internal/orchestration/kubernetes/` (diagnose_pod,
     find_pods_by_service, BuildClient) and
     `internal/versioning/kubernetes/` (get_image_provenance — it's
     versioning, not orchestration).
   - `internal/provider/loki/` → `internal/logs/loki/`
   - `internal/provider/prometheus/` → `internal/metrics/prometheus/`
   - `internal/provider/jaeger/` → `internal/tracing/jaeger/`
   - `internal/provider/falco/` → `internal/security/falco/`
   - `internal/provider/sentry/` → `internal/alerts/sentry/`
   - `internal/provider/pagerduty/` → `internal/alerts/pagerduty/`
   - `internal/provider/argocd/` → `internal/versioning/argocd/`
   - `internal/provider/github/` → `internal/versioning/github/`
   - `internal/provider/gitlab/` → `internal/versioning/gitlab/`
   - `internal/provider/runes/` → `internal/correlation/runes/`
3. **Write the new AlertManager provider** under `internal/alerts/alertmanager/`.
   At this stage it just exposes the same tool shape PagerDuty/Sentry do
   today (`list_active_alerts` etc.) under their pre-refactor names so
   the binary keeps working. The interface unification comes in Étape 3.
4. Extract shared code into `pkg/` :
   - `internal/dispatcher` → `pkg/dispatcher`
   - `internal/provider/provider.go` (base `Provider` + `ToolDefinition`)
     → `pkg/provider`
   - `parseLogLevel` + slog init → `pkg/logging`
   - Inline MCP server construction (currently in
     `internal/app/wiring.go`) → `pkg/mcp` helpers
   - Common `doJSON` + status mapping + trailing-slash trim
     → `pkg/restclient` (each client wraps it with its own auth hook)
5. `internal/app/wiring.go` updates its imports to the new paths but
   still registers the same tools, same env vars, same warn-and-skip.
6. `cmd/rikugan/main.go` is unchanged (already minimal post-PR #10).
7. **Gate** : `go build ./...` + `go vet ./...` + `go test -race ./...`
   all green. Manual smoke test : run the binary, list its tools, expect
   the same 24 names.

### Étape 3 — Concept interfaces and abstraction validation

Goal : prove the concept abstraction is sound before we commit to
splitting binaries.

1. Define interfaces in `pkg/provider/` :
   - `OrchestrationProvider` : `ListServices`, `GetServiceState`,
     `DescribeService`, `GetRecentEvents`, `RestartService`,
     `GetServiceHistory`.
   - `LogsProvider` : `GetLogs`, `TailLogs`, `SearchLogs`.
   - `MetricsProvider` : `GetMetric`, `CompareToBaseline`,
     `ListAvailableMetrics`.
   - `AlertsProvider` : `ListActiveAlerts`, `GetAlertDetails`,
     `AckAlert`, `ResolveAlert`, `SilenceAlert`.
   - (Tracing/Security/Versioning/Correlation : interface definition
     can wait until they have at least two implementations; document
     the methods we'd want as a comment for now.)
2. Refactor each P0 provider to implement its concept interface :
   - `internal/orchestration/kubernetes` implements `OrchestrationProvider`.
   - `internal/logs/loki` implements `LogsProvider`.
   - `internal/metrics/prometheus` implements `MetricsProvider`.
   - `internal/alerts/{alertmanager,sentry,pagerduty}` each implement
     `AlertsProvider`.
3. Replace tool registration : each concept exposes its own tools
   (`get_logs`, `get_metric`, `list_active_alerts`, …) and routes to
   the right provider via config (env var like `LOGS_PROVIDER=loki`).
   Tool names are now concept-named ; rename table goes in `MIGRATION.md`.
4. **Critical abstraction test** : write `internal/orchestration/dockercompose`
   implementing `OrchestrationProvider`. If we have to deform the
   interface (add k8s-specific methods, leave some methods as no-ops),
   STOP : something is wrong with the abstraction. Surface it for
   discussion before continuing.
5. **Gate** : same build/vet/test sweep + a smoke test with both
   `ORCHESTRATION_PROVIDER=kubernetes` and `ORCHESTRATION_PROVIDER=dockercompose`
   that proves the same `list_services` tool works against both
   backends.

### Étape 4 — Split into multiple binaries

Goal : each concept becomes its own MCP server ; the unified one stays
for back-compat.

1. Create `cmd/orchestration-mcp/main.go`, `cmd/logs-mcp/main.go`,
   `cmd/metrics-mcp/main.go`, `cmd/alerts-mcp/main.go`. Each `main.go`
   uses `pkg/logging`, `pkg/mcp`, and `internal/<concept>` to build a
   server that registers only its concept's tools.
2. Keep `cmd/rikugan/main.go` as the unified binary — registers every
   concept in one process. This is the back-compat path for users that
   don't want to run four sidecars.
3. `scripts/build-all.sh` builds the lot. Makefile gets a `make build-all`
   target.
4. README rewrite : new architecture, per-binary usage, unified usage,
   provider-routing config.
5. **Gate** : `make build-all`, each binary starts and reports its tools,
   unified binary still reports the union.

---

## 5. Risks and open questions

- **Alerts ≠ Errors ≠ Incidents.** The user's directive is to put
  AlertManager + Sentry + PagerDuty under the same `Alerts` concept.
  They're sibling-but-distinct domains :
  - AlertManager = alert rules firing on metrics
  - Sentry = application exceptions with deduplication + stack traces
  - PagerDuty = incident management + on-call rotation
  
  Forcing them all to implement the same `AlertsProvider` interface
  may pull either the interface toward the lowest common denominator
  or require provider-specific methods that leak. **Mitigation** :
  define the interface narrowly (`ListActiveAlerts`, `GetAlertDetails`,
  `Ack`, `Resolve`, `Silence`) which all three can express, and keep
  provider-specific tools (`get_current_oncalls` for PagerDuty) outside
  the interface as concept-level optional tools. We re-evaluate at the
  Étape 3 gate ; if the abstraction tears, we may need a separate
  `Errors` concept for Sentry.

- **Versioning is P1 but contains tools we ship today** (argocd,
  github, gitlab). The intent is that this concept stays *structurally*
  ready (folders exist, code lives there) but the interface definition
  + tool rename can wait until P1 work begins. Étape 2 moves the code ;
  Étape 3 is allowed to skip the versioning concept's interface
  refactor.

- **Correlation has no provider abstraction** by design. `runes` is a
  consumer that calls k8s + prom directly. Once Étape 3 lands the
  concept interfaces, runes should be refactored to call
  `OrchestrationProvider` + `MetricsProvider` instead of the k8s
  client and `promv1.API` directly, so it works regardless of which
  providers are selected. Adding this to Étape 3 actions.

- **Docker Compose orchestration provider is new code.** No production
  dependency to wrap, no canonical SDK ; we'll likely shell out to
  `docker compose` and parse output. Risk : the interface methods we
  define (`RestartService`, `GetServiceHistory`) may not have a clean
  Docker Compose mapping. That's precisely what the Étape 3 gate is
  for.

- **AlertManager provider is new code.** Standard REST API, low risk,
  fits the existing `pkg/restclient` pattern.

- **kubectl-logs / docker-logs / kubectl-top providers are new code**
  and are not on the critical path for P0. Marked TODO for follow-up
  PRs after the refactor lands — the concept folders exist, the
  interface fits, implementation can come incrementally.

- **No-breaking-change rule.** Étape 2 preserves every tool name.
  Étape 3 renames many of them. `MIGRATION.md` lists the rename map ;
  consumers (Claude Code config) need a tool-name update at the
  Étape 3 ↔ Étape 4 boundary. We may add temporary alias-registrations
  in the unified binary to soften the transition — decide at Étape 3
  gate.

- **mcp-go is at v0.44.0** in go.mod ; latest is v0.60+ . A bump may
  be useful for newer capability flags during the refactor, but it's
  out of scope here and tracked as a dependabot PR (#17 is open).

---

## 6. Deliverables per stage

| Stage | Deliverable | Validation |
|---|---|---|
| 1 | `REFACTOR_PLAN.md` (this file) | User reads and validates |
| 2 | Reorganised tree, all existing tools still exposed, AlertManager skeleton, shared code in `pkg/` | `go build/vet/test` green ; manual MCP smoke test |
| 3 | Concept interfaces, P0 providers implement them, Docker Compose provider as abstraction test, `MIGRATION.md` | Both `kubernetes` and `dockercompose` work behind the same `Orchestration` interface ; build/vet/test green |
| 4 | One binary per concept, unified binary retained, `scripts/build-all.sh`, README rewrite | All binaries build + start ; tool inventory matches the per-concept partitioning |

---

## 7. Approval checklist

Please respond to confirm or adjust before I start Étape 2 :

- [ ] Concept catalogue (orchestration / logs / metrics / alerts /
      tracing / security / versioning / correlation) is the right shape ?
- [ ] Putting Sentry + PagerDuty + AlertManager under one `AlertsProvider`
      is OK as the starting point, with the fallback of splitting them
      out if the Étape 3 interface tears (mitigation above) ?
- [ ] Mapping `kubernetes.get_image_provenance` to Versioning rather than
      Orchestration ?
- [ ] AlertManager provider written in Étape 2 ; Docker Compose written
      in Étape 3 ; kubectl-logs / docker-logs / kubectl-top deferred to
      follow-up PRs after the refactor ?
- [ ] Tool-name renames acceptable at the Étape 3 ↔ Étape 4 boundary,
      documented in `MIGRATION.md` ?
- [ ] Any tools intentionally left as concept-specific-only outside the
      interface (e.g. PagerDuty's `get_current_oncalls`) ?
