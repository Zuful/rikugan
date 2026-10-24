# Rikugan

**Agentic DevOps for the AI-native era — organised by operational concept, not by technology.**

Rikugan is an MCP server that gives AI agents deep, multi-domain observability across your stack. It is built around four **operational concepts** — Orchestration, Logs, Metrics, Alerts — each with a stable interface and one or more interchangeable backend providers (Kubernetes, Docker Compose, Loki, Prometheus, AlertManager, Sentry, PagerDuty…). Three secondary concepts — Versioning, Tracing, Security — still ship their tools as backend-specific (interface unification will come when each has two implementations to keep honest). One cross-concept correlation tool (Runes / `six_eyes`) ties signals together.

Instead of switching dashboards, pasting logs into prompts, or writing glue scripts, your AI agent calls the right concept tool, Rikugan routes it to the configured backend, and the report comes back as Markdown ready for reasoning.

## What is MCP?

The Model Context Protocol is an open standard for connecting AI models to external tools and data sources. Instead of embedding API calls in prompts, MCP defines a structured interface where a server advertises tools with typed schemas, and a client (typically an AI agent) invokes them. Rikugan implements MCP over STDIO transport — it reads JSON-RPC from stdin and writes responses to stdout, making it easy to integrate with any MCP client.

## Architecture

```
┌─────────────┐   stdin/stdout    ┌──────────────────────────────────────────────────┐
│  MCP Client │ ◄── JSON-RPC ───► │                Rikugan MCP Server                │
│ (AI agent)  │                   │                                                  │
└─────────────┘                   │  Concept tools (uniform names per concept)        │
                                  │  ┌────────────┐ ┌──────┐ ┌────────┐ ┌──────┐    │
                                  │  │Orchestration│ │ Logs │ │Metrics │ │Alerts│    │
                                  │  └────────────┘ └──────┘ └────────┘ └──────┘    │
                                  │         │         │         │          │         │
                                  │  Per-concept interface (provider/concept_interfaces.go)
                                  │         │         │         │          │         │
                                  │  Backend providers (interchangeable)              │
                                  │  ┌──────────────┐┌──────┐┌──────────┐┌─────────┐ │
                                  │  │ kubernetes / ││ loki ││prometheus││alertmgr,│ │
                                  │  │docker-compose││      ││          ││sentry,  │ │
                                  │  │              ││      ││          ││pagerduty│ │
                                  │  └──────────────┘└──────┘└──────────┘└─────────┘ │
                                  │                                                  │
                                  │  Backend-specific extras (no concept yet)         │
                                  │   versioning · tracing · security · correlation  │
                                  └──────────────────────────────────────────────────┘
```

Providers translate their backend's native API into the concept's shared types (`provider.LogEntry`, `provider.MetricSample`, `provider.Alert`, `provider.Service`). The concept-level tool registry exposes one uniform set of tool names regardless of backend — an agent invokes `get_metric` whether the backend is Prometheus, kubectl top, or anything else implementing `MetricsProvider`. Logging goes to stderr as structured JSON so it never interferes with the STDIO transport on stdout.

A few backend-specific tools intentionally survive outside the concept interfaces because they add genuinely unique value (Sentry's release attribution, PagerDuty's on-call rotation, Loki's log anomaly analytics, Kubernetes's discovery helper). They are registered alongside the concept tools on the same dispatcher.

Core logic — query building, projection into concept types, report generation — lives in pure functions decoupled from MCP transport. An alternative client can import the providers directly without an MCP server.

### Concept catalogue

The full list of concepts Rikugan models, with priorities and planned backends, is in [docs/ontology.md](./docs/ontology.md). The table below covers the concepts that have an interface today.

| Concept | Interface methods | Backends |
| --- | --- | --- |
| Orchestration | `ListServices` / `GetServiceState` / `DescribeService` / `GetRecentEvents` / `RestartService` / `GetServiceHistory` | kubernetes (default), docker-compose (selectable via `ORCHESTRATION_PROVIDER`) |
| Logs | `GetLogs` / `TailLogs` / `SearchLogs` | loki; without `LOKI_URL`, docker logs when the orchestration backend is docker-compose |
| Metrics | `GetMetric` / `CompareToBaseline` / `ListAvailableMetrics` | prometheus |
| Alerts | `ListActiveAlerts` / `GetAlertDetails` | alertmanager, sentry, pagerduty (one selected as primary via `ALERTS_PROVIDER`; others register their backend-specific extras only) |
| Deployments | `LastRollout` / `WatchRollouts` | kubernetes, docker-compose (follows `ORCHESTRATION_PROVIDER`) |
| Communication | `PostMessage` | telegram, slack — every configured channel receives the proactive mode's notifications; no MCP tool, messages leave from the daemon |

Three concepts ship tools without a concept interface yet (the interface lands once a second backend exists to keep the abstraction honest):

| Concept | Backend(s) today |
| --- | --- |
| Versioning | github, gitlab, argocd |
| Tracing | jaeger |
| Security | falco |
| Correlation | runes (`six_eyes`), deploy health (`check_deploy_health`), image provenance (`get_image_provenance`) — cross-concept tools, by design without an interface |
| Proactive mode | `get_watch_reports`: verdicts recorded by `rikugan serve`, read through its API |

See [MIGRATION.md](./MIGRATION.md) for the mapping from the pre-refactor technology-prefixed tool names (`query_range`, `diagnose_pod`, `alertmanager_list_active_alerts`, …) to the current concept tools.

## Per-concept binaries

Rikugan ships **five binaries** — the unified one plus one per P0 concept — so operators can run only the slice they need. Each is a standalone MCP server over STDIO with its own name reported to the client.

| Binary | Setup function | When to use |
| --- | --- | --- |
| `rikugan` | `app.Setup` | Single-process deployment — every available provider registers, including cross-concept Runes. Default for single-user setups. |
| `rikugan-orchestration` | `app.SetupOrchestration` | Just the orchestration concept. Honors `ORCHESTRATION_PROVIDER` (`kubernetes` default, `dockercompose`). Useful when running multiple sidecars or when the agent only needs cluster introspection. |
| `rikugan-logs` | `app.SetupLogs` | Just the logs concept. |
| `rikugan-metrics` | `app.SetupMetrics` | Just the metrics concept. |
| `rikugan-alerts` | `app.SetupAlerts` | Just the alerts concept. Honors `ALERTS_PROVIDER`. |

Build them all with `make build-all` (or `./scripts/build-all.sh`). The unified binary stays the back-compat path for users who don't want to run four sidecars.

## Tools

Tools are grouped by concept. **Concept tools** (uniform across backends) come first; **backend-specific tools** that survive outside the strict-intersection interface come after with the rationale for why they stay backend-specific.

### Orchestration concept

Backends: `kubernetes` (default), `dockercompose` (selectable via `ORCHESTRATION_PROVIDER`).

#### `list_services`

Lists every service managed by the configured orchestration backend. For Kubernetes a *service* is a Deployment; for Docker Compose it is a service from the Compose project. Pass an optional `namespace` to scope.

| Parameter   | Type   | Required | Description                                                |
|-------------|--------|----------|------------------------------------------------------------|
| `namespace` | string | no       | Kubernetes namespace / Docker Compose project. Empty = all. |

#### `get_service_state`

Returns a health snapshot for a service: status (`running` / `degraded` / `failed` / `pending` / `unknown`), available vs desired replicas, and a human-readable message. The Kubernetes backend derives status from deployment conditions; the Docker Compose backend aggregates per-container state and health-check results.

| Parameter   | Type   | Required | Description                                                 |
|-------------|--------|----------|-------------------------------------------------------------|
| `name`      | string | yes      | Service name (Deployment for k8s, service name for compose). |
| `namespace` | string | no       | Namespace / project.                                        |
| `kind`      | string | no       | `deployment` (default for k8s), `pod`, `docker-compose-service`. |

#### `describe_service`

Returns the Markdown equivalent of `kubectl describe` / `docker compose inspect`: image, replicas, strategy, restart policy, labels, container statuses.

| Parameter   | Type   | Required | Description           |
|-------------|--------|----------|-----------------------|
| `name`      | string | yes      | Service name.         |
| `namespace` | string | no       | Namespace / project.  |
| `kind`      | string | no       | Service kind.         |

#### `get_recent_events`

Returns the noteworthy orchestration events attached to the service in the lookback window (restarts, scheduling failures, OOM kills, image pull errors). For Kubernetes this filters the events API down to the service's pods; for Docker Compose it shells out to `docker events --since … --until …`.

| Parameter   | Type   | Required | Description                                       |
|-------------|--------|----------|---------------------------------------------------|
| `name`      | string | yes      | Service name.                                     |
| `namespace` | string | no       | Namespace / project.                              |
| `kind`      | string | no       | Service kind.                                     |
| `since`     | string | no       | Lookback duration, e.g. `15m`, `1h`. Default `1h`. |

#### `restart_service`

Triggers a rolling restart. For Kubernetes that patches the deployment with the standard `kubectl.kubernetes.io/restartedAt` annotation; for Docker Compose it runs `docker compose restart <service>`. **Mutating** — make sure the operator intends it. It is the only tool that changes anything, and `RIKUGAN_READ_ONLY=true` leaves it out.

| Parameter   | Type   | Required | Description           |
|-------------|--------|----------|-----------------------|
| `name`      | string | yes      | Service name.         |
| `namespace` | string | no       | Namespace / project.  |
| `kind`      | string | no       | Service kind.         |

#### `get_service_history`

Returns the rolling-update history. The Kubernetes backend lists every ReplicaSet the deployment has owned, sorted by revision, with the image and change-cause for each. The Docker Compose backend has no native equivalent and returns a documented degradation report listing currently-deployed images and pointing the operator at the container registry, the compose-file git history, and CI build logs.

| Parameter   | Type   | Required | Description           |
|-------------|--------|----------|-----------------------|
| `name`      | string | yes      | Service name.         |
| `namespace` | string | no       | Namespace / project.  |
| `kind`      | string | no       | Service kind.         |

#### `find_pods_by_service_name`  *(backend-specific — kubernetes only)*

Bridges a logical service name (Jaeger trace identifier, human-typed name) to concrete Kubernetes pod names. Searches by common labels (`app`, `app.kubernetes.io/name`) first, then falls back to pod-name prefix matching.

| Parameter      | Type   | Required | Description                                       |
|----------------|--------|----------|---------------------------------------------------|
| `service_name` | string | yes      | Logical service name (e.g. from a Jaeger trace).  |
| `namespace`    | string | no       | Kubernetes namespace (default: all namespaces).   |

**Why backend-specific:** Docker Compose service names map 1:1 to container names — no discovery helper is needed there.

### Logs concept

Backend: `loki`. Without `LOKI_URL`, the docker-compose orchestration backend serves these tools from `docker logs`: what the daemon kept of each existing container's output, `query` and `match` being plain substrings, case aside.

#### `get_logs`

Fetch log lines for a service over a time window. At least one of `service`, `namespace`, or `query` is required to scope the call. *[LEST: Errors signal.]*

| Parameter   | Type   | Required | Description                                                  |
|-------------|--------|----------|--------------------------------------------------------------|
| `service`   | string | no       | Service name (mapped to the backend's primary label — `app` for Loki). |
| `namespace` | string | no       | Kubernetes namespace.                                        |
| `query`     | string | no       | Backend-native query expression (a full LogQL selector for Loki, a substring for docker) — overrides label-based selector building when set. |
| `start`     | string | no       | RFC3339 start (default: 1 hour ago).                          |
| `end`       | string | no       | RFC3339 end (default: now).                                   |
| `limit`     | number | no       | Max lines (default 100, max 500).                             |

#### `tail_logs`

Convenience wrapper for the most recent N lines.

| Parameter   | Type   | Required | Description                                                  |
|-------------|--------|----------|--------------------------------------------------------------|
| `service`   | string | no       | Service name.                                                |
| `namespace` | string | no       | Namespace.                                                   |
| `query`     | string | no       | Native query expression overriding service/namespace selection. |
| `lines`     | number | no       | Recent lines to fetch (default 100, max 500).                |

#### `search_logs`

Search logs matching a free-text expression or a native backend query, scoped by service / namespace / labels. Use this to find specific error messages across services. *[LEST: Errors (and Traffic when querying access-log volume).]*

| Parameter   | Type   | Required | Description                                                  |
|-------------|--------|----------|--------------------------------------------------------------|
| `match`     | string | no       | Substring or backend-native expression (e.g. a LogQL selector for Loki). |
| `service`   | string | no       | Service name to scope the search.                            |
| `namespace` | string | no       | Namespace.                                                   |
| `start`     | string | no       | RFC3339 start (default: 1 hour ago).                          |
| `end`       | string | no       | RFC3339 end (default: now).                                   |
| `limit`     | number | no       | Max matches (default 100, max 500).                           |

#### `detect_log_anomalies`  *(backend-specific — loki only)*

Analyzes recent logs for a service and detects anomalies. Classifies log lines by level, counts error/warning rates, and groups error messages by normalized pattern.

| Parameter   | Type   | Required | Description                                       |
|-------------|--------|----------|---------------------------------------------------|
| `service`   | string | yes      | Service name (used as the `app` label in LogQL).  |
| `namespace` | string | no       | Kubernetes namespace to scope the query.          |
| `duration`  | string | no       | Lookback duration (e.g. `1h`, `30m`). Default: `1h`. |

Returns a level distribution table (ERROR, WARN, INFO, DEBUG counts and percentages), a summary sentence, top error patterns grouped by normalized message (timestamps, UUIDs, and numbers stripped), and sample error lines. Analysis is based on up to 1000 most recent lines.

**Why backend-specific:** The pattern-grouping and level-classification analytics are loki-specific synthesis, outside the strict `LogsProvider` interface.

### Metrics concept

Backend: `prometheus`.

#### `get_metric`

Evaluate a query against the metrics backend over a time window and return series + data points. *[LEST: any signal depending on the query — histogram_quantile latency, 5xx rate, CPU/memory saturation, request-rate traffic.]*

| Parameter | Type   | Required | Description                                  |
|-----------|--------|----------|----------------------------------------------|
| `query`   | string | yes      | Backend-native query (PromQL for prometheus). |
| `start`   | string | no       | RFC3339 start (default: 1 hour ago).          |
| `end`     | string | no       | RFC3339 end (default: now).                   |
| `step`    | string | no       | Step duration, e.g. `60s`, `5m`. Default `60s`. |

The handler caps the return at ~10,000 data points to keep agent context manageable; pass a coarser `step` or narrower range if you hit the cap.

#### `compare_to_baseline`

Run the same query for the most recent `window` and for a baseline window ending `baseline` ago, then report each side's mean plus the delta. Useful for spotting regressions ("the p95 just got 30% worse vs 24h ago").

| Parameter  | Type   | Required | Description                                            |
|------------|--------|----------|--------------------------------------------------------|
| `query`    | string | yes      | Backend-native query.                                  |
| `window`   | string | no       | Both windows' width (e.g. `15m`, `1h`). Default `15m`. |
| `baseline` | string | no       | How far back the baseline window ends. Default `24h`.  |

#### `list_available_metrics`

List metric names known to the backend, optionally filtered by prefix. Useful for discovering what to query.

| Parameter | Type   | Required | Description                              |
|-----------|--------|----------|------------------------------------------|
| `prefix`  | string | no       | Case-sensitive metric-name prefix.       |
| `limit`   | number | no       | Max names returned (default 200, max 2000). |

### Alerts concept

Backends: `alertmanager`, `sentry`, `pagerduty`. Pick the primary via `ALERTS_PROVIDER` (default: first available in priority order `alertmanager > sentry > pagerduty`). Non-primary backends still register their backend-specific extras.

#### `list_active_alerts`

List currently-active alerts via the configured backend. The `service` argument is interpreted per backend: AlertManager filters by the `service` label; Sentry treats it as a project slug; PagerDuty looks it up via `FindServiceByName`. *[Methodology: Incident context, pre-Step 0.]*

| Parameter | Type   | Required | Description                                 |
|-----------|--------|----------|---------------------------------------------|
| `service` | string | no       | Service / project / scope key — see above.  |

#### `get_alert_details`

Fetch the full detail of a single alert. The `alert_id` is the backend-native identifier — AlertManager fingerprint, Sentry numeric issue ID, PagerDuty incident ID.

| Parameter  | Type   | Required | Description                                            |
|------------|--------|----------|--------------------------------------------------------|
| `alert_id` | string | yes      | Backend-native identifier (fingerprint / issue ID / incident ID). |

#### `list_issues_for_release`  *(backend-specific — sentry only)*

Lists unresolved Sentry issues first seen in a specific release. Answers "what did this deploy break?" in one call.

| Parameter | Type   | Required | Description                                         |
|-----------|--------|----------|-----------------------------------------------------|
| `version` | string | yes      | Release version (Git SHA, semver tag, or build ID). |
| `limit`   | number | no       | Maximum issues to return (default 25, max 100).     |

**Why backend-specific:** Release attribution is a Sentry-unique signal — no analog in AlertManager or PagerDuty.

#### `get_current_oncalls`  *(backend-specific — pagerduty only)*

Lists who is currently on-call across the org, grouped by escalation policy and ordered by escalation level (primary first).

| Parameter           | Type   | Required | Description                            |
|---------------------|--------|----------|----------------------------------------|
| `escalation_policy` | string | no       | Substring filter on the policy name.   |

**Why backend-specific:** On-call rotation is unique to PagerDuty.

### Versioning (no concept interface yet)

All tools here are backend-specific. A concept interface will land when a second versioning backend is added (e.g. Bitbucket).

#### `get_recent_commits` · `get_commit_diff`  *(github)*

Fetches recent commits or the full diff for a specific commit, with per-file patches.

| Tool | Required parameters |
|---|---|
| `get_recent_commits` | `owner`, `repo` (+ optional `branch`, default `main`) |
| `get_commit_diff` | `owner`, `repo`, `commit_sha` |

Diffs are capped at 10,000 characters.

#### `gitlab_get_recent_commits` · `gitlab_get_commit_diff`  *(gitlab)*

Equivalents for GitLab-hosted repositories, including self-hosted instances.

| Tool | Required parameters |
|---|---|
| `gitlab_get_recent_commits` | `project_id` (numeric or URL-encoded path), optional `branch` |
| `gitlab_get_commit_diff` | `project_id`, `commit_sha` |

#### `get_app_status` · `list_degraded_apps`  *(argocd)*

GitOps sync/health introspection.

| Tool | Required parameters |
|---|---|
| `get_app_status` | `app_name` |
| `list_degraded_apps` | none (optional `project` filter) |

`list_degraded_apps` surfaces every application that is `OutOfSync`, `Degraded`, or `Missing` across the org.

### Tracing (no concept interface yet)

Backend: `jaeger`.

#### `find_slow_traces`

| Parameter      | Type   | Required | Description                                                     |
|----------------|--------|----------|-----------------------------------------------------------------|
| `service`      | string | yes      | Service name to search traces for.                              |
| `min_duration` | string | no       | Minimum trace duration (e.g. `500ms`, `1s`, `2.5s`). Default: none. |

Returns up to 5 traces with trace ID, start time, total duration, and span count. 24-hour lookback.

#### `analyze_trace`

Synthesises a bottleneck report from a Jaeger trace instead of returning raw span JSON.

| Parameter  | Type   | Required | Description              |
|------------|--------|----------|--------------------------|
| `trace_id` | string | yes      | Jaeger trace ID.         |

Parses every span, identifies the 3 slowest and any errored spans, returns total duration, a natural-language bottleneck summary, a ranked table of slowest spans, and an error table.

### Security (no concept interface yet)

Backend: `falco`.

#### `get_security_events`

| Parameter   | Type   | Required | Description                                                          |
|-------------|--------|----------|----------------------------------------------------------------------|
| `namespace` | string | no       | Kubernetes namespace filter.                                         |
| `pod_name`  | string | no       | Pod name filter.                                                     |
| `severity`  | string | no       | Minimum severity (`Notice` / `Warning` / `Error` / `Critical`). Default `Warning`. |

Up to 50 events from the last 24 hours.

#### `analyze_pod_audit`

Groups Falco events for a pod by rule name (e.g. *"3× Terminal shell in container"*, *"1× Write below binary dir"*) and surfaces critical/error counts up front.

| Parameter   | Type   | Required | Description                                 |
|-------------|--------|----------|---------------------------------------------|
| `pod_name`  | string | yes      | Pod name to analyze.                        |
| `namespace` | string | no       | Recommended for precision.                  |

### Correlation

#### `get_image_provenance`  *(correlation — kubernetes only for now)*

Bridges running pods to source code by extracting container image tags and identifying Git SHAs. It reads deployment state and answers in versioning terms, so it lives in the correlation layer rather than in either concept (see [docs/ontology.md](./docs/ontology.md)).

| Parameter   | Type   | Required | Description                     |
|-------------|--------|----------|---------------------------------|
| `namespace` | string | yes      | Kubernetes namespace.           |
| `pod_name`  | string | yes      | Name of the pod to inspect.     |

Parses each container's image reference into registry, repository, and tag. If a tag matches a Git SHA pattern (7–40 hex characters), it's flagged and the report includes an explicit pointer to `get_commit_diff`. Covers both regular and init containers.

#### `six_eyes`  *(runes — cross-concept tool, unified binary only)*

The correlation engine. Combines pod diagnostics with targeted Prometheus queries to produce a ranked diagnosis.

| Parameter   | Type   | Required | Description                                  |
|-------------|--------|----------|----------------------------------------------|
| `namespace` | string | yes      | Kubernetes namespace.                        |
| `pod`       | string | yes      | Pod name.                                    |
| `window`    | string | no       | Metrics look-back window. Default `1h`.      |

1. Fetches the pod and its events.
2. Builds PromQL queries based on pod state. Three base queries always run (CPU usage rate, memory working set, restart count). Conditional queries are added when relevant — OOMKill reason, memory limits, and CPU throttling when restarts or OOM are detected; node pressure conditions when the pod is pending.
3. Runs all queries concurrently.
4. Applies correlation heuristics:

| Signal              | Severity | Trigger                                                |
|---------------------|----------|--------------------------------------------------------|
| `OOM_KILL`          | Critical | Container terminated with OOMKilled reason             |
| `APPLICATION_CRASH` | High     | Non-zero exit code (not OOM)                           |
| `CPU_STARVATION`    | High     | Restarts combined with CPU throttling                  |
| `NODE_PRESSURE`     | High     | Pod pending with node resource pressure                |
| `HEALTHY`           | Info     | All containers ready, no issues detected               |

Findings are sorted by severity, followed by pod summary, events, and raw metric data. Available only in the unified `rikugan` binary — runes spans the Orchestration and Metrics concepts simultaneously and intentionally does not fit a single per-concept binary.

#### `check_deploy_health`  *(deploy health — cross-concept tool, unified binary only)*

Did the service's last rollout break something? Reads the Deployments, Orchestration and Metrics concepts through their interfaces; the rules live in the pure `internal/health` package.

| Parameter   | Type   | Required | Description                         |
|-------------|--------|----------|-------------------------------------|
| `name`      | string | yes      | Service (Deployment) name.          |
| `namespace` | string | yes      | Kubernetes namespace.               |

The minutes after the rollout completed — up to the latest checkpoint reached, +5, +15 or +30 min — are compared with the 30 minutes before it started:

| Signal | Investigate | Degraded |
|---|---|---|
| Latency (p95 of each window's requests) | +30 % and +50 ms | +100 % and +200 ms, from +15 only (capped at investigate at +5: warm-up) |
| 5xx rate | ×2, > 1 %, ≥ 10 errors | ×2, > 5 %, ≥ 25 errors |
| Restarts (new revision) | 1 restart | attested OOM kill, crash loop, or ≥ 2 restarts |
| Availability | — | an instance unready past its grace (60 s for the rollout's own, 2 min for later ones) |
| Saturation (per replica, from +15) | +40 % | +100 % |
| Traffic | off by −50 % / +200 % against both the before window and a week earlier, or collapsed | — |

Latency is the p95 of all the requests of each window, from the histogram buckets (`histogram_quantile` over `increase` across the window, template `latency_window_p95`). `latency_method: median` takes instead the median of the per-minute p95 (template `latency_p95`), which a few warm-up minutes cannot drag but which is not a p95 of the window; it is also what applies when the window template is empty. Latency and error rate are only evaluated with at least 200 requests in each window. Below that, at +5 and +15, a blunt error regression (5xx above 20 % with at least 10 errors) is still `degraded` (`blunt_rule: false` turns this off); otherwise the verdict is `insufficient_data` unless another signal fires. At +5 and +15 the blunt rule stays out when the service was already above 20 % 5xx before the rollout: those errors are not the deploy's doing. Prometheus has no 5xx series until a first 5xx is served, so a window without 5xx data is read as zero errors when the other window has some, and the report says so (`0 errors before: no 5xx series before the rollout`); after the rollout this also needs the request series to be there, proof the target is scraped. One case this cannot tell apart: a rollout that renames the status label or the error metric looks like a drop to zero errors. Without any request metric, the error rate is never reported as 0 %: it is `insufficient_data`. An `ok` verdict needs latency or error rate to be evaluable, and the report then lists every signal it could not evaluate. A stalled rollout (Kubernetes `ProgressDeadlineExceeded`) is `degraded`. A rollout still in progress or completed under 5 minutes ago gets no verdict, but its new revision's restarts and readiness are reported; one completed over 2 hours ago gets no verdict.

Works on both orchestration backends (`ORCHESTRATION_PROVIDER=kubernetes` or `dockercompose`; on Compose, `namespace` is the project) and needs a metrics backend. The default PromQL templates assume `http_requests_total` / `http_request_duration_seconds` with `namespace` and `service` labels, and cAdvisor container metrics; on Compose, where scrape labels depend on your setup, override them. Thresholds and templates can be overridden per service with a YAML file named by `RIKUGAN_HEALTH_CONFIG`:

```yaml
defaults:
  thresholds:
    min_requests: 100
  templates:
    request_rate: sum(rate(http_server_requests_seconds_count{namespace="{{.Namespace}}",app="{{.Service}}"}[1m]))
services:
  prod/checkout:
    thresholds:
      rollout_unavailable_grace: 90s
```

Templates are Go templates over `{{.Namespace}}`, `{{.Service}}` and `{{.Revision}}`, written in the metrics backend's query language, and must return one aggregated series. Unknown keys in the file are rejected.

### Proactive mode

#### `get_watch_reports`

What has `rikugan serve` found lately? Lists the verdicts the daemon recorded after each rollout (+5, +15, +30 min checkpoints, and failed rollouts) and in its periodic sweep, newest first, with the signals that were not ok. Registered when `RIKUGAN_SERVE_URL` points at a running daemon (see [Proactive mode](#proactive-mode-rikugan-serve-in-progress)).

| Parameter | Type   | Required | Description |
|-----------|--------|----------|-------------|
| `service` | string | No       | `namespace/name`, or a bare name for every namespace; empty for all services |
| `since`   | string | No       | A duration such as `6h`, or an RFC 3339 time (default `24h`) |
| `limit`   | number | No       | Maximum number of verdicts (default 50) |

## Getting Started

### Prerequisites

- Go 1.26+
- At least one configured backend. For the orchestration concept that means either a Kubernetes cluster (kubeconfig or in-cluster) or a Docker Compose project, depending on `ORCHESTRATION_PROVIDER`.

Every other backend is optional. Unset env vars degrade gracefully — the matching tools simply don't register and Rikugan logs the skip at startup.

### Build

```sh
# Unified binary only
go build -o rikugan ./cmd/rikugan

# All five binaries (unified + per-concept) into bin/
make build-all
```

### Run

The unified binary:

```sh
# Kubernetes-only setup (orchestration concept, no other backends)
./rikugan

# Full stack: every backend wired
KUBECONFIG=~/.kube/config \
  LOKI_URL=http://loki:3100 \
  PROMETHEUS_URL=http://prometheus:9090 \
  ALERTMANAGER_URL=http://alertmanager:9093 \
  SENTRY_URL=https://sentry.io SENTRY_AUTH_TOKEN=sntrys_... SENTRY_ORG=acme \
  PAGERDUTY_API_TOKEN=u+... \
  GITHUB_TOKEN=ghp_... \
  GITLAB_URL=https://gitlab.example.com GITLAB_TOKEN=glpat-... \
  ARGOCD_SERVER=https://argocd.example.com ARGOCD_TOKEN=eyJ... \
  JAEGER_URL=http://jaeger-query:16686 \
  FALCO_URL=http://falcosidekick:2801 \
  ./rikugan
```

The per-concept binaries:

```sh
# Just orchestration, k8s backend
KUBECONFIG=~/.kube/config ./bin/rikugan-orchestration

# Just orchestration, docker-compose backend (no kubeconfig needed)
ORCHESTRATION_PROVIDER=dockercompose COMPOSE_PROJECT_NAME=mystack ./bin/rikugan-orchestration

# Just logs (loki only)
LOKI_URL=http://loki:3100 ./bin/rikugan-logs

# Just metrics (prometheus only)
PROMETHEUS_URL=http://prometheus:9090 ./bin/rikugan-metrics

# Just alerts, pagerduty as primary
ALERTS_PROVIDER=pagerduty \
  PAGERDUTY_API_TOKEN=u+... \
  ALERTMANAGER_URL=http://alertmanager:9093 \
  ./bin/rikugan-alerts
```

### Proactive mode: `rikugan serve` (in progress)

`rikugan serve` is the daemon mode of the same binary: it follows the rollouts of the watched namespaces and runs the `check_deploy_health` rules on each one at +5, +15 and +30 minutes after it completes, and at once when it fails. Every rollout, verdict and skipped checkpoint is recorded in an append-only journal (`verdicts.jsonl`) under the state directory, from which it resumes after a restart without running a check twice; checkpoints missed while it was down are summed up in one late check. When rollouts follow each other closely, a rollout's before window ends where the last stable period did, at the start of the first rollout of the chain; when that is more than `stale_after` (2 h) back, it is no credible baseline, so latency, error rate, saturation and traffic are not compared with it, and only the blunt error rule and the instance signals apply. Each verdict is also written to stdout as one JSON line, for log pipelines.

```bash
RIKUGAN_SERVE_NAMESPACES=shop,payments \
  PROMETHEUS_URL=http://prometheus:9090 \
  RIKUGAN_STATE_DIR=/var/lib/rikugan \
  ./bin/rikugan serve
```

It runs as a **single instance**: a lock on the state directory makes a second one refuse to start, but that lock is unreliable on network file systems, where the state directory must not live. Rollouts are detected on the orchestration backend, Kubernetes or Docker Compose. On Compose, a rollout is a new `config-hash` on a service's containers (named by its first 12 characters: Compose numbers no revision), complete once they are all ready (healthy, or running without a healthcheck) and no older one runs. The daemon records the completion time when it sees it; a later reconstruction (`check_deploy_health`, a catch-up after downtime) may find that Docker's healthcheck log, which keeps 5 probes, no longer holds the first passing one, and then takes the container's start, a stable lower bound, and says the time is estimated; one not complete after `RIKUGAN_COMPOSE_ROLLOUT_DEADLINE` (default 10 min) is failed. It is packaged for Kubernetes (the Helm chart, `serve.enabled`, see [Helm](#helm)), Docker Compose ([`deploy/compose`](./deploy/compose/compose.yaml)) and systemd ([`deploy/systemd`](./deploy/systemd/rikugan-serve.service)).

**On Docker Compose**, the daemon runs in its own container from the `serve-compose` image target (`docker build --target serve-compose -t rikugan:serve-compose .`), which adds the Docker CLI, and watches another project (`WATCHED_PROJECT`) through the Docker socket. Access to the socket is root on the host: the daemon only runs read commands, and [`compose.socket-proxy.yaml`](./deploy/compose/compose.socket-proxy.yaml) puts it behind a read-only socket proxy (containers and events, no write), so that the socket is no longer mounted into the daemon. `container_name` is fixed so that a second instance cannot start.

**To try it on your machine**, [`deploy/lab`](./deploy/lab/README.md) runs a small shop under real traffic with a Prometheus and `rikugan serve` watching it: `./lab.sh deploy web errors` (or `slow`, `crash`, `oom`, `leak`…) deploys a bad revision, and `./lab.sh export` gathers the verdicts next to what was expected of each.

**With systemd**, `rikugan-serve.service` is a plain unit (not a template, single instance), with `StateDirectory=rikugan` for the journal, a dedicated user and systemd hardening; settings go in `/etc/rikugan/serve.env` ([example](./deploy/systemd/serve.env.example)).

**Checking the setup: `rikugan doctor`.** It reads the environment as `rikugan serve` does and checks it end to end, as a checklist (✓ fine, ! worth a look, ✗ serve would not run or would run blind; exit code 1 on any ✗):

- the verdicts API's listen/token rule;
- the state directory (writable, and whether a running daemon holds it);
- the health settings;
- the metrics backend;
- the watched namespaces and their services;
- on Kubernetes, the RBAC rights the Helm chart's Role grants;
- each service's current rollout, and each of its query templates, sent as a dry run over the last hour. A template the backend refuses fails. One that selects nothing, often a misspelt metric or label name that the daemon cannot tell from a quiet service, or that returns several series, is a warning;
- the notification channels, the alerts backend, the heartbeat, the sweep and Compose settings.

`rikugan doctor --send-test` also posts a test message on every channel and pings the heartbeat. Run it where the daemon runs, with its environment:

- `kubectl exec deploy/<release>-rikugan-serve -- /rikugan doctor` (the chart's serve Deployment);
- `docker exec rikugan-serve rikugan doctor`;
- with systemd, `sudo -u rikugan sh -c 'set -a; . /etc/rikugan/serve.env; rikugan doctor'`.

**Periodic sweep.** Every 4 hours (`RIKUGAN_SWEEP_INTERVAL`, `off` to disable), every service of the swept namespaces (`RIKUGAN_SWEEP_NAMESPACES`, default the watched ones; `RIKUGAN_SWEEP_EXCLUDE` leaves some out) is checked for what rollouts do not catch: a slow degradation, a dependency going down, a memory leak. The last hour is compared with the same hour a week earlier (a day earlier when the week is missing); restarts count since the previous sweep (on a service's first sweep, a lifetime restart count says nothing about now, so only a crash loop in progress and terminations in the last hour, attested OOM kills included, count); traffic only flags a collapse; a memory leak needs a steady rise of at least 25 % and 100 MiB per replica that traffic does not explain. Services with a rollout under 2 hours old are left to the rollout checks. Services that reach the same event in one sweep share one message (fifteen services degraded by a shared dependency send one critical listing them, not fifteen). Per service, a degraded sweep is notified `critical` once per incident and "resolved" after two OK sweeps in a row; an investigate verdict is only notified once it has lasted three sweeps in a row (`RIKUGAN_SWEEP_WARNING_PERSISTENCE`), or at once with `RIKUGAN_SWEEP_NOTIFY_WARNING=true`.

**When a backend fails.** A check that cannot read the orchestration backend is retried every minute; with only the metrics backend failing, restarts and availability are still evaluated, and a degraded verdict on them is notified at once (recorded as `partial`, the checkpoint is checked in full once the metrics are back). Checks still failing past `stale_after` are recorded as not evaluated and given up, and the team gets one warning per outage ("rikugan cannot evaluate: metrics backend prometheus failing since …"), then an "evaluates again" message when it is back; the outage's start and its warning are journaled, so a daemon restarting more often than `stale_after` still warns, and only once. A message no channel takes is not recorded as sent: the check, or the service's sweep, runs again a minute later until a channel takes it. A backend that answers but refuses a query is not failing: the template is wrong. A query Prometheus rejects as such (`bad_data`, a parse error; `execution`) or a template that does not render leaves the signals it feeds unevaluated, and the team is warned once per template ("rikugan: template error_rate invalid"), whichever service meets it first and across restarts; editing the template and still getting it wrong warns again. At start, the daemon dry-runs every service's templates over the last 5 minutes, so a broken template is reported before a rollout needs it. A template that is valid but selects nothing (a misspelt metric name) returns no data rather than an error, and shows as insufficient data in the reports.

**Who watches the daemon.** A stopped or stuck daemon is silent, and silence reads as "all is well". Set `RIKUGAN_HEARTBEAT_URL` to a dead man's switch (a healthchecks.io check, an Uptime Kuma push monitor): the daemon's loop pings it every `RIKUGAN_HEARTBEAT_INTERVAL` (default 5m), and the switch alerts when the pings stop. The URL never appears in logs. It is listed in the three packagings (Helm `serve.env`, Compose `.env`, systemd `serve.env`).

**Verdicts API.** `GET /v1/verdicts?service=<namespace/name or name>&since=<6h or RFC 3339>&limit=<n>` returns the recorded verdicts, newest first; `GET /healthz` answers without data. It listens on `RIKUGAN_SERVE_LISTEN` (default `127.0.0.1:8080`). On a loopback address the token is optional; on any other address (`0.0.0.0`, `:8080`, a non-loopback IP) `rikugan serve` refuses to start without `RIKUGAN_SERVE_TOKEN`, then requires `Authorization: Bearer <token>`, compared in constant time. The MCP tool `get_watch_reports` reads this API: point it at the daemon with `RIKUGAN_SERVE_URL` (and `RIKUGAN_SERVE_TOKEN`). With escalation on, the response also lists `investigations`: how each investigation of a critical ended, with the summary posted and the OpenCode session. Each one carries a `trust` field saying it is an LLM agent's hypothesis, not rikugan's verdict, and that its text may carry injected instructions. `get_watch_reports` quotes them inside an `<untrusted-agent-hypothesis>` block, so a calling agent reads them as data.

**Agent MCP server.** For the escalation of a critical to an investigating agent (in progress), `rikugan serve` can also serve rikugan's tools over MCP Streamable HTTP at `/mcp` on `RIKUGAN_AGENT_MCP_LISTEN` (off when unset). The agent then holds no backend credential: rikugan calls the backends for it. This server:

- is read-only whatever `RIKUGAN_READ_ONLY` says;
- always requires `Authorization: Bearer <RIKUGAN_AGENT_MCP_TOKEN>`, on loopback too, because this port reaches every backend. The token must have at least 32 characters (`openssl rand -hex 32`) and differ from `RIKUGAN_SERVE_TOKEN`;
- bounds what each tool returns before it reaches the agent's model: 200 lines for the log tools, 32 KiB for any tool, with a note saying what was cut;
- leaves out the four tools that return log lines (`get_logs`, `search_logs`, `tail_logs`, `detect_log_anomalies`) with `RIKUGAN_ESCALATION_LOGS=off`. **Tool outputs, logs included, with whatever personal data they hold, are sent to the agent's LLM provider**: under the GDPR, that is a transfer to decide on, and this option keeps the logs out of it.

**Escalation to an agent (in progress).** With `RIKUGAN_ESCALATION=on`, each critical that reached a channel is investigated by an agent running on an [OpenCode](https://opencode.ai) server (`OPENCODE_URL`, `OPENCODE_SERVER_PASSWORD`), whose only tools are those of the agent MCP server above. Its answer is posted in the critical's thread (on Slack, with a bot; a webhook quotes the critical instead). The agent only reads and suggests: rikugan never acts on what it says.

- **The answer is checked before it is posted.** The agent answers with a JSON object following rikugan's schema, in its last message; an agent that ends without one is reminded once, within the same bounds.
  - The suggested action is one of rollback, restart, scale, inspect, escalate or none, and its target must belong to the incident; otherwise it is rejected.
  - Each quoted piece of evidence must appear in the output of a tool call the agent really made; otherwise it is marked unverified.
  - High confidence without verified evidence is lowered.
  - Commands, links and secrets are removed from the text.
  - The message always ends with: "Hypothesis generated by an LLM from untrusted data. Verify before acting."
- **The OpenCode server is checked before each investigation.** Its API can run commands, so the password is required. Escalation is blocked, with one warning to the team, if any of these happens:
  - one of its native tools is unknown to rikugan;
  - its tool list cannot be read;
  - an MCP server other than rikugan's is configured;
  - rikugan's MCP server is not connected, even after rikugan asks OpenCode to reconnect it (OpenCode does not retry by itself: rikugan serve down when OpenCode started, or restarted since).
- **Bounds.** One investigation per incident, which a sweep's grouped critical counts as one. The other limits are configurable:
  - `RIKUGAN_ESCALATION_PER_HOUR`: investigations started per hour, default 4;
  - `RIKUGAN_ESCALATION_CONCURRENCY`: investigations running at once, default 1;
  - `RIKUGAN_ESCALATION_MONTHLY_BUDGET`: spending per calendar month, default $5. It is rebuilt from the journal at start, and tokens whose price OpenCode does not know count at $15 per million;
  - per investigation: 10 minutes (`RIKUGAN_ESCALATION_TIMEOUT`), 40 tool calls (`RIKUGAN_ESCALATION_MAX_CALLS`) and 200,000 tokens (`RIKUGAN_ESCALATION_MAX_TOKENS`);
  - an investigation still waiting 30 minutes after its critical is dropped.

  A cap reached, a timeout, a failure, or a restart during an investigation (never run again) each leave a line in the thread.
- `RIKUGAN_ESCALATION_TEAMS` lists the teams an "escalate" suggestion may name.

For a local model, the lab runs the escalation on Ollama, with an injection canary: see [deploy/lab](deploy/lab/README.md#7-escalation-an-agent-investigates-each-critical).

**Packaging the escalation.** OpenCode runs apart from rikugan serve, never as a sidecar: a sidecar would share the pod's network (the verdicts API on loopback) and often its ServiceAccount token.

- **Helm.** Set `serve.escalation.enabled=true` with `serve.escalation.opencode.model` and the LLM provider's key (`serve.escalation.opencode.envFrom`). The chart then adds:
  - an OpenCode Deployment with the pinned image, no ServiceAccount token, non-root, and a read-only file system;
  - its generated config: rikugan's MCP server alone, every native tool off and denied;
  - a Secret, generated once and kept, holding the OpenCode password and the agent MCP token. With GitOps, use `serve.escalation.existingSecret`;
  - two NetworkPolicies:
    - OpenCode answers rikugan serve alone. It reaches only DNS, the agent MCP port, and HTTPS outside private ranges, so it has no path into the cluster.
    - The agent MCP port answers OpenCode alone.

  A NetworkPolicy filters addresses, not domain names, so "the LLM provider alone" needs an egress proxy or an FQDN policy where the CNI has one (Cilium).
- **Compose.** Add [`compose.escalation.yaml`](./deploy/compose/compose.escalation.yaml). Its isolation is narrower, and the file says how.
- **Personal data.** Tool outputs, logs included, go to the LLM provider. Two ways keep them in:
  - `serve.escalation.logs=false` (`RIKUGAN_ESCALATION_LOGS=off`) withholds the log tools;
  - a self-hosted model (Ollama, vLLM) keeps everything inside. `serve.escalation.networkPolicy.llmEndpoint` opens the one exception to the private ranges it needs, as a CIDR or a pod and namespace selector, with its ports.

**Notifications** go to every configured channel (Telegram, Slack; see `TELEGRAM_*` and `SLACK_*`): a degraded verdict or a failed rollout is `critical`, an investigate verdict a `warning`; OK verdicts are only sent with `RIKUGAN_NOTIFY_ON_OK=true`. Each rollout gets one notification, and a later checkpoint only notifies again when the verdict gets worse, with one exception: after a critical, the first checkpoint back below degraded sends a single "recovered" message (insufficient data never counts as recovered). When an alerts backend is configured, alerts already firing for the service are mentioned in the message.

### Docker

```sh
docker build -t rikugan:latest .
```

Multi-stage build (Go 1.27 Alpine builder, distroless runtime), runs as non-root.

### Helm

```sh
helm install rikugan helm/rikugan \
  --set prometheus.url=http://prometheus:9090
```

The chart creates a ServiceAccount and ClusterRole with read access to pods, pod logs, and events. Both are optional (`--set serviceAccount.create=false --set rbac.create=false`).

`--set serve.enabled=true` also deploys `rikugan serve`, the proactive mode's daemon:

```sh
helm install rikugan helm/rikugan \
  --set prometheus.url=http://prometheus:9090 \
  --set serve.enabled=true \
  --set 'serve.namespaces={shop,payments}' \
  --set 'serve.envFrom[0].secretRef.name=rikugan-notifications'   # TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID…
```

It runs as a single instance by construction: `replicas: 1` is fixed in the template (not a value) and the strategy is `Recreate`, so an upgrade never runs two daemons on the journal. The journal lives on a `ReadWriteOnce` PVC (`serve.persistence`). Each watched namespace (default: the release namespace) gets a read-only Role. The verdicts API is exposed by a Service and requires a bearer token, generated once into the Secret `<release>-rikugan-serve` (key `token`) and kept across `helm upgrade`; `helm install` prints how to fetch it and port-forward for `get_watch_reports`. **With GitOps (Argo CD, Flux), set `serve.auth.existingSecret`**: they render the chart without cluster access, so the generated token would change on every sync. `serve.healthConfig` takes the per-service thresholds file.

### Claude Desktop

Add to your Claude Desktop MCP configuration (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "rikugan": {
      "command": "/path/to/rikugan",
      "env": {
        "KUBECONFIG": "/path/to/.kube/config",
        "PROMETHEUS_URL": "http://localhost:9090",
        "LOKI_URL": "http://localhost:3100",
        "ALERTMANAGER_URL": "http://localhost:9093",
        "ALERTS_PROVIDER": "alertmanager",
        "GITHUB_TOKEN": "ghp_...",
        "GITLAB_URL": "https://gitlab.example.com",
        "GITLAB_TOKEN": "glpat-...",
        "ARGOCD_SERVER": "https://argocd.example.com",
        "ARGOCD_TOKEN": "eyJ...",
        "JAEGER_URL": "http://jaeger-query:16686",
        "FALCO_URL": "http://falcosidekick:2801"
      }
    }
  }
}
```

You can also wire one entry per per-concept binary if you prefer to keep them isolated.

### Configuration

| Environment Variable        | Purpose                                                       | Required |
|-----------------------------|---------------------------------------------------------------|----------|
| `KUBECONFIG`                | Path to kubeconfig file                                       | When orchestration backend is `kubernetes` (default) |
| `ORCHESTRATION_PROVIDER`    | `kubernetes` (default) or `dockercompose`                     | No        |
| `COMPOSE_PROJECT_NAME`      | Compose project name (when `ORCHESTRATION_PROVIDER=dockercompose`) | No |
| `LOKI_URL`                  | Loki API URL                                                  | When logs concept is wired (with docker-compose, docker logs serve it otherwise) |
| `PROMETHEUS_URL`            | Prometheus server URL                                         | When metrics concept is wired |
| `ALERTS_PROVIDER`           | `alertmanager` / `sentry` / `pagerduty` — selects primary for `list_active_alerts` / `get_alert_details` | No (defaults to first available, priority `alertmanager > sentry > pagerduty`) |
| `ALERTMANAGER_URL`          | AlertManager API URL                                          | When alertmanager backend is wired |
| `SENTRY_URL`                | Sentry base URL                                               | When sentry backend is wired (defaults to `https://sentry.io`) |
| `SENTRY_AUTH_TOKEN`         | Sentry auth token                                             | When sentry backend is wired |
| `SENTRY_ORG`                | Sentry org slug                                               | When sentry backend is wired |
| `PAGERDUTY_API_TOKEN`       | PagerDuty API token                                           | When pagerduty backend is wired |
| `GITHUB_TOKEN`              | GitHub personal access token                                  | When github tools are wired |
| `GITLAB_TOKEN`              | GitLab personal access token                                  | When gitlab tools are wired |
| `GITLAB_URL`                | GitLab instance URL                                           | No (defaults to `https://gitlab.com`) |
| `ARGOCD_SERVER`             | Argo CD server URL                                            | When argocd tools are wired |
| `ARGOCD_TOKEN`              | Argo CD API bearer token                                      | When argocd tools are wired |
| `JAEGER_URL`                | Jaeger Query API URL                                          | When jaeger tools are wired |
| `FALCO_URL`                 | Falcosidekick URL                                             | When falco tools are wired |
| `TELEGRAM_BOT_TOKEN`        | Telegram bot token (from @BotFather)                          | For proactive-mode notifications to Telegram (read by `rikugan serve`, in progress) |
| `TELEGRAM_CHAT_ID`          | Chat or channel the bot posts to                              | With `TELEGRAM_BOT_TOKEN` |
| `SLACK_WEBHOOK_URL`         | Slack incoming-webhook URL (https)                            | For proactive-mode notifications to Slack (read by `rikugan serve`, in progress) |
| `SLACK_BOT_TOKEN`           | Slack bot token (`xoxb-…`, scope `chat:write`), instead of the webhook: only a bot can answer a message in its thread (a webhook reply quotes the message instead). Setting both is refused | Alternative to `SLACK_WEBHOOK_URL` |
| `SLACK_CHANNEL`             | ID of the channel the bot posts to (`C…`); invite the bot to it | With `SLACK_BOT_TOKEN` |
| `RIKUGAN_STATE_DIR`         | `rikugan serve`: journal and lock directory (local disk, not NFS) | No (defaults to `/var/lib/rikugan`) |
| `RIKUGAN_SERVE_NAMESPACES`  | `rikugan serve`: comma-separated namespaces (Compose projects on Docker Compose) whose rollouts are watched | Outside a pod (in a pod, defaults to its own namespace; on Compose, to `COMPOSE_PROJECT_NAME`) |
| `RIKUGAN_COMPOSE_ROLLOUT_DEADLINE` | Docker Compose: time after which a revision whose containers are not all ready is failed | No (defaults to `10m`) |
| `RIKUGAN_SERVE_LISTEN`      | `rikugan serve`: verdicts API address                         | No (defaults to `127.0.0.1:8080`) |
| `RIKUGAN_SERVE_TOKEN`       | Bearer token of the verdicts API; read by `rikugan serve` and by `get_watch_reports` | Required when `RIKUGAN_SERVE_LISTEN` is not loopback |
| `RIKUGAN_SERVE_URL`         | Address of a running `rikugan serve`, for `get_watch_reports` | When the tool is wanted |
| `RIKUGAN_SWEEP_INTERVAL`    | `rikugan serve`: periodic sweep interval, or `off`            | No (defaults to `4h`) |
| `RIKUGAN_SWEEP_NAMESPACES`  | `rikugan serve`: namespaces swept, comma-separated            | No (defaults to the watched namespaces) |
| `RIKUGAN_SWEEP_EXCLUDE`     | `rikugan serve`: services left out of the sweep (`name` or `namespace/name`) | No |
| `RIKUGAN_SWEEP_NOTIFY_WARNING` | `rikugan serve`: notify every sweep warning (`true`)       | No (defaults to off: only persistent warnings) |
| `RIKUGAN_SWEEP_WARNING_PERSISTENCE` | `rikugan serve`: consecutive investigate sweeps before a warning is sent | No (defaults to `3`) |
| `RIKUGAN_NOTIFY_ON_OK`      | `rikugan serve`: also notify OK verdicts (`true`)             | No (defaults to off) |
| `RIKUGAN_AGENT_MCP_LISTEN`  | `rikugan serve`: address of the agent MCP server (in progress) | No (off when unset) |
| `RIKUGAN_AGENT_MCP_TOKEN`   | Bearer token of the agent MCP server, 32 characters or more, distinct from `RIKUGAN_SERVE_TOKEN` | With `RIKUGAN_AGENT_MCP_LISTEN` |
| `RIKUGAN_ESCALATION_LOGS`   | `off` keeps the log tools from the agent MCP server           | No (defaults to `on`) |
| `RIKUGAN_ESCALATION`        | `on`: an agent investigates each delivered critical (in progress) | No (defaults to off) |
| `OPENCODE_URL`              | OpenCode server the investigations run on                     | With `RIKUGAN_ESCALATION=on` |
| `OPENCODE_SERVER_PASSWORD`  | That server's password                                        | With `RIKUGAN_ESCALATION=on` |
| `RIKUGAN_ESCALATION_PER_HOUR`, `_CONCURRENCY`, `_MONTHLY_BUDGET`, `_TIMEOUT`, `_MAX_CALLS`, `_MAX_TOKENS`, `_TEAMS` | Escalation bounds (see [Proactive mode](#proactive-mode-rikugan-serve-in-progress)) | No (4, 1, $5, 10m, 40, 200000, none) |
| `RIKUGAN_READ_ONLY`         | `true` registers only the tools that read (today, every tool but `restart_service`), whatever the backends' credentials allow. Any value other than `true` or `false` refuses to start | No (defaults to `false`) |
| `RIKUGAN_LOG_LEVEL`         | Log verbosity: `debug` / `info` / `warn` / `error`            | No (defaults to `info`) |

> **Graceful by design.** Missing config = skipped backend. Rikugan logs which providers were activated and which were skipped at startup, so you always know what's available.

## Migration from the pre-refactor tool names

Tool names changed when the codebase moved from technology-first to concept-first. The full old → new mapping is in [`MIGRATION.md`](./MIGRATION.md) — read it before updating an existing MCP client configuration (Claude Desktop, IDE, etc.).

Quick highlights:

- `query_range` → `get_metric`
- `diagnose_pod` → compose `describe_service` + `get_service_state` + `get_recent_events`
- `alertmanager_list_active_alerts` / `list_recent_issues` / `list_active_incidents` → `list_active_alerts` (concept tool, primary chosen via `ALERTS_PROVIDER`)
- `get_issue_detail` → `get_alert_details`
- `get_log_context` → `get_logs` with explicit `start`/`end`

## Investigation Methodology

Rikugan's tools are designed to be used together, not in isolation. The
playbook the agent should follow when invoked to investigate an incident
is documented in [`docs/methodology.md`](docs/methodology.md) and centres
on the **LEST** mnemonic — Latency, Errors, Saturation, Traffic (the
Four Golden Signals from Google SRE, reordered to be memorable).

Each tool's MCP description references its role in this methodology, so a
calling agent knows when to reach for each one. Read the doc once to
internalise the flow ("Step 0: what changed?", LEST, correlation, blast
radius, read-only first) — it will make the agent's investigations
materially more structured.

## Use Cases

### Incident Triage

An on-call engineer tells the agent: *"The checkout service is returning 500s."* The agent calls `list_active_alerts` to confirm whether AlertManager / Sentry / PagerDuty already track an incident, then `six_eyes` on the checkout pod. Rikugan detects OOMKilled containers, correlates with memory metrics showing usage hitting limits, and returns a `CRITICAL: OOM_KILL` finding with the specific container and memory data. The agent can immediately suggest increasing memory limits or investigating the memory leak.

### Debugging a Failing Service

The agent calls `describe_service` for the full deployment spec, `get_service_state` for the replica counts and current status, and `get_recent_events` to see scheduling failures, OOM kills, or backoff intervals — three concept tools that work the same way against Kubernetes and Docker Compose backends.

### Capacity Investigation

A platform team wants to understand resource consumption across services. The agent calls `get_metric` with a CPU or memory PromQL expression scoped to a namespace, reviews the time-series data, and identifies pods that are consistently over- or under-provisioned. Follow up with `compare_to_baseline` to spot regressions vs the previous day.

### Pending Pod Analysis

A deployment is stuck — pods won't schedule. The agent calls `six_eyes`, which detects the Pending phase and automatically queries node pressure conditions. If nodes are under memory or disk pressure, it surfaces a `NODE_PRESSURE` finding with the specific condition, pointing the team toward cluster scaling or workload redistribution.

### Change Correlation

A pod starts crash-looping after a deployment. The agent calls `get_image_provenance` to extract the container image tag, recognises it as a Git SHA, and calls `get_commit_diff` to inspect the code change. Combined with `six_eyes`, it can correlate the crash reason (e.g. OOMKill) with a specific code change (e.g. a removed memory limit or a new unbounded cache) and point the engineer directly to the relevant diff.

### GitOps Drift Detection

An engineer asks: *"Is everything in sync?"* The agent calls `list_degraded_apps` to scan all Argo CD applications. Any app that is `OutOfSync` or `Degraded` is surfaced immediately. The agent can then call `get_app_status` on a specific app to get the full details and suggest a remediation (manual sync, rollback, or code fix).

### Latency Investigation

A user reports that API responses are slow. The agent calls `find_slow_traces` for the `api-gateway` service with `min_duration: "2s"`. It finds several slow traces, then calls `analyze_trace` on the worst one. The report reveals that 80% of the trace duration is spent in a `db_query` span in the `payment-db` service. The agent then calls `find_pods_by_service_name` to locate the `payment-db` pods and `describe_service` / `get_service_state` to check their state — completing the investigation across three concepts in seconds.

### Security Forensics

A pod is behaving normally by all metrics but the team suspects compromise. The agent calls `analyze_pod_audit` on the pod. Falco reveals *"3x Terminal shell in container"* and *"1x Write below binary dir"* — someone exec'd into the container and modified a binary. The grouped summary immediately highlights the anomaly without flooding the context with hundreds of raw events.

### Log Investigation

An engineer sees a spike in 500s. The agent calls `find_slow_traces` for the `api-gateway` service, finds a trace with errors at `2024-01-15T10:32:00Z`. The agent then calls `get_logs` with that service and the timestamp window to retrieve the surrounding log lines. The logs reveal a database connection timeout error right before the trace failure. The agent calls `detect_log_anomalies` on `api-gateway` and discovers that "connection timeout" errors account for 40% of recent ERROR lines — confirming a systemic issue rather than a one-off failure.

### Release Attribution

A spike of errors lands in production right after a deploy. The agent calls `list_issues_for_release` with the new version. Sentry returns every issue first seen with that release. The agent then calls `get_commit_diff` on the corresponding Git SHA to inspect the offending change.

### Proactive Health Checks

An agent on a scheduled loop calls `six_eyes` across key pods in a namespace. Healthy pods return an `INFO: HEALTHY` finding and are skipped. Any pod with warnings, restarts, or throttling gets flagged for review before it becomes an incident.

## License

See [LICENSE](LICENSE).
