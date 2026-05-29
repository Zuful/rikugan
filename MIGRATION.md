# Rikugan MCP — Étape 3 migration guide

This document maps the old (technology-first) MCP tool names to the
new (concept-first) names introduced in **étape 3** of the refactor
documented in [REFACTOR_PLAN.md](./REFACTOR_PLAN.md).

Étape 3 introduced six concept interfaces in
[`pkg/provider/concept_interfaces.go`](./pkg/provider/concept_interfaces.go)
and reorganised the tool surface so that every operationally-meaningful
question routes through a backend-agnostic concept tool. Backends still
exist as separate packages (`internal/<concept>/<backend>/`) and host
the genuinely backend-specific tools that don't project into the
strict-intersection interfaces.

If you previously called a tool that no longer exists, look it up in
the **Removed tools** table to find its concept-level replacement.

---

## Tool count

| Étape | Tools | Notes |
| --- | --- | --- |
| Pre-étape 3 | 25 | Per-backend tool surface |
| Post-étape 3 | 30 | +5 net: 13 backend tools collapsed into 8 concept tools (`get_logs`, `tail_logs`, `search_logs`, `get_metric`, `compare_to_baseline`, `list_available_metrics`, `list_active_alerts`, `get_alert_details`, `list_services`, `get_service_state`, `describe_service`, `get_recent_events`, `restart_service`, `get_service_history`) |

---

## New concept tools

The following tools are new in étape 3. Each routes through one of the
concept interfaces (`LogsProvider`, `MetricsProvider`, `AlertsProvider`,
`OrchestrationProvider`) and accepts a uniform argument shape regardless
of the backend wired underneath.

### Logs concept (`internal/logs`)

| Tool | Replaces | Selected backend |
| --- | --- | --- |
| `get_logs` | `search_logs` (loki) — when invoked without a substring filter | `LOKI_URL` → loki |
| `tail_logs` | (new) | `LOKI_URL` → loki |
| `search_logs` | `search_logs` (loki) | `LOKI_URL` → loki |

### Metrics concept (`internal/metrics`)

| Tool | Replaces | Selected backend |
| --- | --- | --- |
| `get_metric` | `query_range` (prometheus) | `PROMETHEUS_URL` → prometheus |
| `compare_to_baseline` | (new — built from two `GetMetric` calls) | `PROMETHEUS_URL` → prometheus |
| `list_available_metrics` | (new — `LabelValues(__name__)`) | `PROMETHEUS_URL` → prometheus |

### Alerts concept (`internal/alerts`)

| Tool | Replaces | Selected backend |
| --- | --- | --- |
| `list_active_alerts` | `alertmanager_list_active_alerts`, `list_recent_issues` (sentry), `list_active_incidents` (pagerduty) | `ALERTS_PROVIDER` env var (default: first available; priority `alertmanager > sentry > pagerduty`) |
| `get_alert_details` | `get_issue_detail` (sentry) | same selector |

`service` argument mapping per backend:
- AlertManager — used as a label filter (`service="<value>"`)
- Sentry — used as the project slug
- PagerDuty — used as the service name (looked up via `FindServiceByName`)

### Orchestration concept (`internal/orchestration`)

| Tool | Replaces | Selected backend |
| --- | --- | --- |
| `list_services` | (new — composes deployments/services) | kubernetes only today |
| `get_service_state` | part of `diagnose_pod` (replicas + status) | kubernetes only today |
| `describe_service` | part of `diagnose_pod` (spec + status) | kubernetes only today |
| `get_recent_events` | part of `diagnose_pod` (events section) | kubernetes only today |
| `restart_service` | (new — kubectl rollout restart equivalent) | kubernetes only today |
| `get_service_history` | (new — ReplicaSet revisions) | kubernetes only today |

A second `OrchestrationProvider` implementation exists in
`internal/orchestration/dockercompose/` as proof the interface
generalises. It is **not wired** into the default startup; the brief's
chunk-E validation showed the interface holds with a documented
soft tear on `GetServiceHistory` (Docker Compose has no native
rolling-update history; the provider returns a fallback report listing
currently-deployed images and pointing the operator at registry tags,
git log, and CI build logs).

---

## Removed tools (and their replacements)

| Removed tool | Concept replacement | Reason |
| --- | --- | --- |
| `query_range` (prometheus) | `get_metric` | Same shape, concept-level naming |
| `get_log_context` (loki) | `get_logs` with explicit `start`/`end` | The "context window" was just a time-range fetch |
| `alertmanager_list_active_alerts` | `list_active_alerts` (concept) | Backend prefix dropped; routed via concept |
| `list_recent_issues` (sentry) | `list_active_alerts` (concept) | `service` arg becomes project slug |
| `get_issue_detail` (sentry) | `get_alert_details` (concept) | `alert_id` arg = sentry numeric issue ID |
| `list_active_incidents` (pagerduty) | `list_active_alerts` (concept) | Same call, concept-level naming |
| `list_service_incidents` (pagerduty) | `list_active_alerts` (concept, with `service`) | Service-scoped view via the concept |
| `diagnose_pod` (kubernetes) | `describe_service` + `get_service_state` + `get_recent_events` | Compound tool decomposed — call the three in sequence (or just the one you need) |

---

## Backend-specific tools that survive

Each backend keeps the tools that genuinely add value beyond the strict
intersection of its concept interface. These are not renamed — the
backend prefix (when present) is preserved so calls remain unambiguous.

| Tool | Backend | Why it stays |
| --- | --- | --- |
| `detect_log_anomalies` | loki | Analytics tool (level classification, pattern grouping) outside `LogsProvider` |
| `list_issues_for_release` | sentry | Release attribution — no analog in AlertManager or PagerDuty |
| `get_current_oncalls` | pagerduty | On-call rotation — no analog in AlertManager or Sentry |
| `find_pods_by_service_name` | kubernetes (orchestration) | Discovery helper — Docker Compose's service name = container name needs no analog |
| `get_image_provenance` | kubernetes (versioning) | Walks a pod's image tags for Git SHAs — bridges orchestration to versioning |
| `analyze_pod_audit` | falco | Kubernetes audit-log analysis specific to falco |
| `get_security_events` | falco | Falco runtime alerts — no concept interface defined yet |
| `find_slow_traces`, `analyze_trace` | jaeger | Tracing concept interface not yet defined |
| `get_recent_commits`, `get_commit_diff` | github | Versioning concept interface not yet defined |
| `gitlab_get_recent_commits`, `gitlab_get_commit_diff` | gitlab | Versioning concept interface not yet defined |
| `get_app_status`, `list_degraded_apps` | argocd | Versioning concept interface not yet defined |
| `six_eyes` | runes (correlation) | Cross-signal correlation tool — its own concept |

---

## Configuration changes

| Env var | Purpose | Default |
| --- | --- | --- |
| `LOKI_URL` | logs concept backend | (required) |
| `PROMETHEUS_URL` | metrics concept backend | (required) |
| `ALERTS_PROVIDER` | which alerts backend serves `list_active_alerts` / `get_alert_details` | first available, priority `alertmanager > sentry > pagerduty` |
| `ALERTMANAGER_URL` | enables alertmanager backend | (unset = skipped) |
| `SENTRY_URL` + `SENTRY_AUTH_TOKEN` + `SENTRY_ORG` | enables sentry backend | (unset = skipped) |
| `PAGERDUTY_API_TOKEN` | enables pagerduty backend | (unset = skipped) |
| (existing) `KUBECONFIG`, `GITHUB_TOKEN`, `GITLAB_URL` + `GITLAB_TOKEN`, `ARGOCD_SERVER` + `ARGOCD_TOKEN`, `JAEGER_URL`, `FALCO_URL` | unchanged | — |

---

## What étape 4 will add (preview, not delivered yet)

- `ORCHESTRATION_PROVIDER` env var (kubernetes / docker-compose) to
  pick the orchestration backend at runtime
- `LOGS_PROVIDER` for kubectl-logs / docker-logs alongside loki
- Per-concept binaries: `cmd/orchestration-mcp/`, `cmd/logs-mcp/`,
  `cmd/metrics-mcp/`, `cmd/alerts-mcp/` sharing the `pkg/` and
  `internal/<concept>/` packages.

See [REFACTOR_PLAN.md](./REFACTOR_PLAN.md) for the master plan.
