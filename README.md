# Rikugan

**Agentic DevOps for the AI-native era.**

Rikugan is an MCP server that gives AI agents deep, multi-domain observability across your entire stack. It exposes eighteen tools — spanning Kubernetes diagnostics, Prometheus metrics, GitHub and GitLab change history, Argo CD deployment state, Jaeger distributed traces, Falco runtime security, and Loki centralized log search — over the [Model Context Protocol](https://modelcontextprotocol.io). One binary, one config, seven domains of perception.

Instead of switching between dashboards, pasting logs into prompts, or writing glue scripts, your AI agent calls the right tool at the right time, correlates signals across domains, and delivers an end-to-end diagnosis in seconds.

## What is MCP?

The Model Context Protocol is an open standard for connecting AI models to external tools and data sources. Instead of embedding API calls in prompts, MCP defines a structured interface where a server advertises tools with typed schemas, and a client (typically an AI agent) invokes them. Rikugan implements MCP over STDIO transport — it reads JSON-RPC from stdin and writes responses to stdout, making it easy to integrate with any MCP client.

## The Seven Eyes

Rikugan's design philosophy is called **The Seven Eyes** — seven domains of perception that together give an AI agent complete situational awareness over a production system. Each Eye is an independent provider that opens a different lens on your infrastructure. When all seven are active, the agent can trace a problem from a user-facing symptom through metrics, code changes, deployment state, request traces, security events, and centralized logs — without human intervention.

| Eye | Domain | Provider | What It Sees |
|-----|--------|----------|--------------|
| :eye: **The Runtime Eye** | Kubernetes | `kubernetes` | Pod state, container logs, events, restarts, scheduling failures |
| :chart_with_upwards_trend: **The Metrics Eye** | Prometheus | `prometheus` + `runes` | CPU, memory, throttling, OOM kills, node pressure — and the correlation engine that ties metrics to pod state |
| :scroll: **The Change Eye** | GitHub / GitLab | `github`, `gitlab` | Recent commits, diffs, code provenance — what changed and when |
| :rocket: **The Deployment Eye** | Argo CD | `argocd` | Sync status, health state, GitOps drift, rollout errors |
| :mag: **The Trace Eye** | Jaeger | `jaeger` | Distributed traces, latency bottlenecks, cross-service error propagation |
| :shield: **The Audit Eye** | Falco | `falco` | Runtime security events — shell access, binary modifications, privilege escalation |
| :bookmark_tabs: **The Log Eye** | Loki | `loki` | Log search, contextual retrieval, anomaly detection |

### Bridging the Domains

Raw tools alone leave semantic gaps between domains. A Jaeger trace says `payment-service` is slow, but Kubernetes knows it as `payment-service-7b4f9c-x2k`. An image tag `a1b2c3d` might be a Git SHA, but only if someone connects the dots.

Rikugan includes **bridge tools** that close these gaps:

- **`find_pods_by_service_name`** — Translates a logical service name (from Jaeger, Argo CD, or a human) into concrete Kubernetes pod names via label matching and prefix fallback.
- **`get_image_provenance`** — Extracts container image tags from a running pod and identifies Git SHAs, pointing the agent directly to `get_commit_diff` for the source code.

These bridges are what turn isolated tools into a connected investigation graph.

## Tools

### Kubernetes Provider

#### `diagnose_pod`

Collects everything you need to understand a pod's state in a single call: status, conditions, container states, recent events, and logs (including previous container logs when restarts have occurred).

| Parameter   | Type   | Required | Description                              |
|-------------|--------|----------|------------------------------------------|
| `namespace` | string | yes      | Kubernetes namespace                     |
| `pod`       | string | yes      | Pod name                                 |
| `tail`      | number | no       | Log lines per container (default 50, max 1000) |

Internally, the tool fetches the pod object first, then fans out to collect events and logs concurrently. If a container has restarted, it retrieves both current and previous logs. The output is a Markdown report with status indicators:

- **Running** — all containers ready
- **Warning** — non-zero exit codes, waiting states
- **CrashLoopBackOff** — restart loop detected

#### `find_pods_by_service_name`

Bridges Jaeger service names to Kubernetes pod names. Finds pods that belong to a logical service.

| Parameter      | Type   | Required | Description                                        |
|----------------|--------|----------|----------------------------------------------------|
| `service_name` | string | yes      | Logical service name (e.g. from a Jaeger trace)    |
| `namespace`    | string | no       | Kubernetes namespace (default: all namespaces)      |

Searches by common labels (`app`, `app.kubernetes.io/name`) first, then falls back to pod name prefix matching if no labels match. The report indicates which match method was used so the LLM knows the confidence level, and suggests next steps (`diagnose_pod` or `six_eyes`).

#### `get_image_provenance`

Bridges running pods to source code by extracting container image tags and identifying Git SHAs.

| Parameter   | Type   | Required | Description                     |
|-------------|--------|----------|---------------------------------|
| `namespace` | string | yes      | Kubernetes namespace            |
| `pod_name`  | string | yes      | Name of the pod to inspect      |

Parses each container's image reference into registry, repository, and tag. If a tag matches a Git SHA pattern (7-40 hex characters), it's flagged and the report includes an explicit suggestion: *"To see the code: use `get_commit_diff` with `commit_sha: a1b2c3d`"*. Covers both regular and init containers.

### Prometheus Provider

#### `query_range`

Executes a PromQL range query against Prometheus and returns formatted time-series data.

| Parameter | Type   | Required | Description                                  |
|-----------|--------|----------|----------------------------------------------|
| `query`   | string | yes      | PromQL expression                            |
| `start`   | string | no       | RFC 3339 timestamp (default: 1 hour ago)     |
| `end`     | string | no       | RFC 3339 timestamp (default: now)            |
| `step`    | string | no       | Step duration, e.g. `60s`, `5m` (default: `60s`) |

A data-point guard rejects queries that would return more than 10,000 samples, preventing excessive data transfer to the agent.

### Runes Provider (Correlation Engine)

#### `six_eyes`

The correlation engine. Combines pod diagnostics with targeted Prometheus queries to produce a ranked diagnosis.

| Parameter   | Type   | Required | Description                                  |
|-------------|--------|----------|----------------------------------------------|
| `namespace` | string | yes      | Kubernetes namespace                         |
| `pod`       | string | yes      | Pod name                                     |
| `window`    | string | no       | Metrics look-back window (default: `1h`)     |

**How it works:**

1. Fetches the pod object and its events from the Kubernetes API.
2. Builds PromQL queries based on pod state. Three base queries always run (CPU usage rate, memory working set, restart count). Conditional queries are added when relevant — OOMKill reason, memory limits, and CPU throttling when restarts or OOM are detected; node pressure conditions when the pod is pending.
3. Executes all queries concurrently against Prometheus.
4. Runs correlation heuristics against the combined data and produces ranked findings:

| Signal | Severity | Trigger |
|--------|----------|---------|
| `OOM_KILL` | Critical | Container terminated with OOMKilled reason |
| `APPLICATION_CRASH` | High | Non-zero exit code (not OOM) |
| `CPU_STARVATION` | High | Restarts combined with CPU throttling |
| `NODE_PRESSURE` | High | Pod pending with node resource pressure |
| `HEALTHY` | Info | All containers ready, no issues detected |

The report presents findings sorted by severity, followed by pod summary, events, and raw metric data for the agent to reason over.

### GitHub Provider

#### `get_recent_commits`

Fetches the most recent commits from a GitHub repository. Useful for correlating code changes with runtime incidents.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `owner`   | string | yes      | GitHub repository owner (user or organisation)   |
| `repo`    | string | yes      | GitHub repository name                           |
| `branch`  | string | no       | Branch name (default: `main`)                    |

Returns a Markdown table with the last 10 commits showing short SHA, author, date, and first line of the commit message.

#### `get_commit_diff`

Fetches the full diff for a specific commit, including per-file patches.

| Parameter    | Type   | Required | Description                                    |
|--------------|--------|----------|------------------------------------------------|
| `owner`      | string | yes      | GitHub repository owner (user or organisation) |
| `repo`       | string | yes      | GitHub repository name                         |
| `commit_sha` | string | yes      | Full or abbreviated commit SHA                 |

Returns commit metadata (SHA, author, date, message) followed by per-file diffs. Output is capped at 10,000 characters to avoid overwhelming the agent context — if the diff exceeds this limit it is truncated with a note indicating the total size.

### GitLab Provider

#### `gitlab_get_recent_commits`

Fetches the most recent commits from a GitLab project. Equivalent to `get_recent_commits` for GitLab-hosted repositories, including self-hosted instances.

| Parameter    | Type   | Required | Description                                                        |
|--------------|--------|----------|--------------------------------------------------------------------|
| `project_id` | string | yes      | GitLab project ID (numeric) or URL-encoded path (e.g. `group/project`) |
| `branch`     | string | no       | Branch name (default: `main`)                                      |

Returns a Markdown table with the last 10 commits showing short SHA, author, date, and commit title.

#### `gitlab_get_commit_diff`

Fetches the full diff for a specific commit from a GitLab project.

| Parameter    | Type   | Required | Description                                                        |
|--------------|--------|----------|--------------------------------------------------------------------|
| `project_id` | string | yes      | GitLab project ID (numeric) or URL-encoded path (e.g. `group/project`) |
| `commit_sha` | string | yes      | Full or abbreviated commit SHA                                     |

Returns commit metadata followed by per-file diffs. Same 10,000-character truncation as the GitHub equivalent.

### Argo CD Provider

#### `get_app_status`

Gets the sync and health status of an Argo CD application.

| Parameter  | Type   | Required | Description                      |
|------------|--------|----------|----------------------------------|
| `app_name` | string | yes      | Name of the Argo CD application  |

Returns a report with sync status (`Synced`, `OutOfSync`), health status (`Healthy`, `Degraded`, `Missing`), tracked repository, target revision, and the last operation details including any error messages.

#### `list_degraded_apps`

Lists all Argo CD applications that are currently Degraded, Missing, or OutOfSync.

| Parameter | Type   | Required | Description                                          |
|-----------|--------|----------|------------------------------------------------------|
| `project` | string | no       | Argo CD project name to filter by (default: all)     |

Returns a summary table of unhealthy applications with their sync status, health status, repository, and error messages. Useful for a quick health check across all GitOps deployments.

### Jaeger Provider

#### `find_slow_traces`

Finds the most recent slow traces for a service from Jaeger.

| Parameter      | Type   | Required | Description                                                     |
|----------------|--------|----------|-----------------------------------------------------------------|
| `service`      | string | yes      | Service name to search traces for                               |
| `min_duration` | string | no       | Minimum trace duration (e.g. `500ms`, `1s`, `2.5s`). Default: no minimum |

Returns a summary table of up to 5 traces with trace ID, start time, total duration, and span count. Searches a 24-hour lookback window.

#### `analyze_trace`

Analyzes a Jaeger trace and returns a synthesized bottleneck report instead of raw span JSON.

| Parameter  | Type   | Required | Description              |
|------------|--------|----------|--------------------------|
| `trace_id` | string | yes      | Jaeger trace ID to analyze |

Parses all spans in the trace, identifies the 3 slowest spans and any spans with errors, and returns a structured report including:
- Total trace duration and span count
- A natural-language bottleneck summary (e.g. *"The bottleneck was `db_query` (1.8s) in service `payment-db`"*)
- Ranked table of slowest spans with service, operation, duration, and percentage of total time
- Error table listing any spans with error tags or log entries

### Falco Provider

#### `get_security_events`

Fetches recent Falco security events from Falcosidekick with optional filtering.

| Parameter   | Type   | Required | Description                                                          |
|-------------|--------|----------|----------------------------------------------------------------------|
| `namespace` | string | no       | Kubernetes namespace to filter events by                             |
| `pod_name`  | string | no       | Pod name to filter events by                                         |
| `severity`  | string | no       | Minimum severity: `Notice`, `Warning`, `Error`, `Critical` (default: `Warning`) |

Returns a table of up to 50 events from the last 24 hours showing time, severity, rule name, and output message. Server-side filtering by priority, client-side filtering by namespace/pod.

#### `analyze_pod_audit`

Analyzes Falco security events for a specific pod and returns a synthesized summary grouped by rule.

| Parameter   | Type   | Required | Description                                 |
|-------------|--------|----------|---------------------------------------------|
| `pod_name`  | string | yes      | Pod name to analyze audit events for         |
| `namespace` | string | no       | Kubernetes namespace (recommended for precision) |

Instead of listing raw events, groups them by rule name (e.g. *"3x Terminal shell in container"*, *"1x Write below binary dir"*) and returns:
- Total event count and unique rule count
- Critical/Error event count highlighted
- Grouped table sorted by severity then frequency, with first/last seen timestamps
- Sample output message for each rule to give the LLM context

### Loki Provider

#### `search_logs`

Searches logs in Loki using a LogQL expression. Returns matching log lines with timestamps and stream labels.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `query`   | string | yes      | LogQL expression (e.g. `{app="payment-service"} |= "error"`) |
| `start`   | string | no       | Start time in RFC3339 format (default: 1 hour ago) |
| `end`     | string | no       | End time in RFC3339 format (default: now)        |
| `limit`   | number | no       | Maximum log lines to return (default: 100, max: 500) |

Returns a Markdown table of log lines with timestamp, stream labels, and the log line (truncated at 200 characters). Results are sorted newest-first.

#### `get_log_context`

Retrieves logs surrounding a specific timestamp for a service. This is a bridge tool — use it when you find a timestamp from a trace or event and want the surrounding log lines for context.

| Parameter   | Type   | Required | Description                                     |
|-------------|--------|----------|-------------------------------------------------|
| `service`   | string | yes      | Service name (used as the `app` label in LogQL) |
| `namespace` | string | no       | Kubernetes namespace to scope the query         |
| `timestamp` | string | yes      | Center timestamp in RFC3339 format              |
| `window`    | string | no       | Time window around the timestamp (e.g. `5m`, `10m`). Default: `5m` |

Returns a metadata header with service, namespace, center timestamp, and window, followed by a table of log lines in the window.

#### `detect_log_anomalies`

Analyzes recent logs for a service and detects anomalies. Classifies log lines by level, counts error/warning rates, and groups error messages by normalized pattern.

| Parameter   | Type   | Required | Description                                     |
|-------------|--------|----------|-------------------------------------------------|
| `service`   | string | yes      | Service name (used as the `app` label in LogQL) |
| `namespace` | string | no       | Kubernetes namespace to scope the query         |
| `duration`  | string | no       | Lookback duration (e.g. `1h`, `30m`). Default: `1h` |

Returns a level distribution table (ERROR, WARN, INFO, DEBUG counts and percentages), a summary sentence, top error patterns grouped by normalized message (timestamps, UUIDs, and numbers stripped), and sample error lines. Analysis is based on up to 1000 most recent lines.

### Sentry Provider

#### `list_recent_issues`

Lists recent Sentry issues for a project, ordered by last-seen. An *issue* is a deduplicated group of similar errors — 1000 occurrences of the same exception collapse into one issue with a count.

| Parameter | Type   | Required | Description                                          |
|-----------|--------|----------|------------------------------------------------------|
| `project` | string | yes      | Sentry project slug                                  |
| `query`   | string | no       | Sentry search query (default: `is:unresolved`)       |
| `limit`   | number | no       | Maximum issues to return (default: 20, max: 100)     |

Returns a Markdown table with short ID, level, occurrence count, users affected, last-seen timestamp, and title. The short ID can be used in the Sentry UI; the numeric ID feeds into `get_issue_detail`.

#### `get_issue_detail`

Fetches the full detail of a single Sentry issue: title, culprit, level, count, first/last seen, the release that first introduced it, and failure metadata (type, function, filename). The release attribution is what makes Sentry uniquely useful — it directly answers *"is this error tied to a recent deploy?"*

| Parameter  | Type   | Required | Description                                              |
|------------|--------|----------|----------------------------------------------------------|
| `issue_id` | string | yes      | Numeric Sentry issue ID (not the short ID like `WEB-A12`) |

When the issue has a `firstRelease`, the report ends with a hint pointing the agent at `get_commit_diff` to inspect the changes shipped in that release.

#### `list_issues_for_release`

Lists unresolved issues first seen in a specific release. Answers *"what did this deploy break?"* in one call.

| Parameter | Type   | Required | Description                                         |
|-----------|--------|----------|-----------------------------------------------------|
| `version` | string | yes      | Release version (Git SHA, semver tag, or build ID)  |
| `limit`   | number | no       | Maximum issues to return (default: 25, max: 100)    |

Returns a Markdown table of new issues for the release, with short ID, level, count, users, project, and title. An empty result means *Sentry-side*, nothing new broke with this release.

### PagerDuty Provider

Orthogonal to the LEST signals — PagerDuty is incident management, not observability. Use it as a pre-investigation step ("is someone already on this?") and as a post-investigation step ("who do I notify?").

#### `list_active_incidents`

Lists incidents currently triggered or acknowledged (not yet resolved), optionally filtered by service name.

| Parameter | Type   | Required | Description                                       |
|-----------|--------|----------|---------------------------------------------------|
| `service` | string | no       | Service name to filter by (exact match)           |

Returns a table with incident number, status, urgency, creation time, service, current assignee, and title.

#### `list_service_incidents`

Lists incidents (any status, including resolved) raised against a service over a lookback window. Surfaces noisy services and recurring patterns.

| Parameter | Type   | Required | Description                                            |
|-----------|--------|----------|--------------------------------------------------------|
| `service` | string | yes      | Service name (exact match)                             |
| `since`   | string | no       | Lookback duration (e.g. `24h`, `7d`). Default: `24h`   |

Includes a status breakdown header (`triggered=2, acknowledged=1, resolved=5`) so the agent gets a one-glance pattern read.

#### `get_current_oncalls`

Lists who is currently on-call across the org, grouped by escalation policy and ordered by escalation level (primary first).

| Parameter           | Type   | Required | Description                                                   |
|---------------------|--------|----------|---------------------------------------------------------------|
| `escalation_policy` | string | no       | Substring filter on the policy name                           |

Returns a section per policy with level, user, schedule, and shift end time.

## Architecture

```
┌─────────────┐   stdin/stdout    ┌──────────────────────────────────────────┐
│  MCP Client  │ ◄──── JSON-RPC ────► │            Rikugan MCP Server            │
│  (AI Agent)  │                  │                                          │
└─────────────┘                  │  ┌──────────┐ ┌──────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐│
                                 │  │Kubernetes│ │Prometheus│ │ GitHub │ │ GitLab │ │Argo CD │ │ Jaeger │ │ Falco  │ │ Loki   ││
                                 │  │ Provider │ │ Provider │ │Provider│ │Provider│ │Provider│ │Provider│ │Provider│ │Provider││
                                 │  └──────────┘ └──────────┘ └────────┘ └────────┘ └────────┘ └────────┘ └────────┘ └────────┘│
                                 │  ┌──────────────────────────────────────┐│
                                 │  │          Runes Provider               ││
                                 │  │       (correlation engine)            ││
                                 │  └──────────────────────────────────────┘│
                                 └──────────────────────────────────────────┘
                                        │                │              │
                                        ▼                ▼              ▼
                                 ┌──────────┐ ┌──────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌──────────────┐
                                 │Kubernetes│ │Prometheus│ │ GitHub │ │ GitLab │ │Argo CD │ │ Jaeger │ │Falcosidekick│ │  Loki  │
                                 │  API     │ │  API     │ │  API   │ │  API   │ │  API   │ │  API   │ │     API      │ │  API   │
                                 └──────────┘ └──────────┘ └────────┘ └────────┘ └────────┘ └────────┘ └──────────────┘ └────────┘
```

Providers are independent modules that register tools with a central dispatcher. The dispatcher wires each tool's schema and handler into the MCP server. Logging goes to stderr as structured JSON so it never interferes with the STDIO transport on stdout.

The correlation engine (Runes) is the only compound tool — it calls both the Kubernetes and Prometheus APIs internally to produce a single correlated diagnosis. All other tools are intentionally isolated: each returns a self-contained report, and the AI agent acts as the orchestration layer, chaining tools together based on what it learns. This is by design — the LLM sees across all domains simultaneously and can make investigation decisions that no single tool could.

Core logic — query building, correlation heuristics, report generation — lives in pure functions decoupled from MCP transport. An alternative client (CLI, web, another agent framework) can import the providers directly without an MCP server.

## Getting Started

### Prerequisites

- Go 1.25+
- Access to a Kubernetes cluster (kubeconfig or in-cluster)

All other providers are optional. Enable them by setting their environment variables.

### Build

```sh
go build -o rikugan ./cmd/rikugan
```

### Run

```sh
# Kubernetes only (diagnose_pod, find_pods_by_service_name, get_image_provenance)
./rikugan

# With Prometheus (adds query_range and six_eyes correlation engine)
PROMETHEUS_URL=http://prometheus:9090 ./rikugan

# With GitHub change intelligence
GITHUB_TOKEN=ghp_... ./rikugan

# With GitLab (supports self-hosted instances)
GITLAB_TOKEN=glpat-... GITLAB_URL=https://gitlab.example.com ./rikugan

# With Argo CD deployment state
ARGOCD_SERVER=https://argocd.example.com ARGOCD_TOKEN=eyJ... ./rikugan

# With Jaeger trace analysis
JAEGER_URL=http://jaeger-query:16686 ./rikugan

# With Falco runtime security
FALCO_URL=http://falcosidekick:2801 ./rikugan

# With Loki log search
LOKI_URL=http://loki:3100 ./rikugan

# All Seven Eyes open
PROMETHEUS_URL=http://prometheus:9090 \
  GITHUB_TOKEN=ghp_... \
  GITLAB_TOKEN=glpat-... GITLAB_URL=https://gitlab.example.com \
  ARGOCD_SERVER=https://argocd.example.com ARGOCD_TOKEN=eyJ... \
  JAEGER_URL=http://jaeger-query:16686 \
  FALCO_URL=http://falcosidekick:2801 \
  LOKI_URL=http://loki:3100 \
  ./rikugan
```

Rikugan reads its Kubernetes configuration from the standard chain: explicit path, `KUBECONFIG` env var, `~/.kube/config`, or in-cluster config.

### Docker

```sh
docker build -t rikugan:latest .
```

The image uses a multi-stage build (Go 1.25 Alpine builder, distroless runtime) and runs as non-root.

### Helm

```sh
helm install rikugan helm/rikugan \
  --set prometheus.url=http://prometheus:9090
```

The chart creates a ServiceAccount and ClusterRole with read access to pods, pod logs, and events. Both are optional:

```sh
helm install rikugan helm/rikugan \
  --set serviceAccount.create=false \
  --set rbac.create=false
```

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
        "GITHUB_TOKEN": "ghp_...",
        "GITLAB_TOKEN": "glpat-...",
        "GITLAB_URL": "https://gitlab.example.com",
        "ARGOCD_SERVER": "https://argocd.example.com",
        "ARGOCD_TOKEN": "eyJ...",
        "JAEGER_URL": "http://jaeger-query:16686",
        "FALCO_URL": "http://falcosidekick:2801",
        "LOKI_URL": "http://loki:3100"
      }
    }
  }
}
```

> **Graceful by design.** Every environment variable above except `KUBECONFIG` is optional. If a token or URL is missing, Rikugan closes that Eye and continues with the rest. Set only what you have — you can start with just Kubernetes and open more Eyes as your stack grows. The server logs which providers were activated and which were skipped at startup, so you always know exactly what's available.

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

An on-call engineer tells the agent: *"The checkout service is returning 500s."* The agent calls `six_eyes` on the checkout pod. Rikugan detects OOMKilled containers, correlates with memory metrics showing usage hitting limits, and returns a `CRITICAL: OOM_KILL` finding with the specific container and memory data. The agent can immediately suggest increasing memory limits or investigating the memory leak.

### Debugging a CrashLoopBackOff

The agent calls `diagnose_pod` and gets the full picture: container exit codes, restart counts, the last few log lines before each crash, and the Kubernetes events showing the backoff intervals. No need for the engineer to run `kubectl describe`, `kubectl logs`, and `kubectl logs --previous` separately.

### Capacity Investigation

A platform team wants to understand resource consumption across services. The agent calls `query_range` with a CPU or memory PromQL expression scoped to a namespace, reviews the time-series data, and identifies pods that are consistently over-provisioned or under-provisioned.

### Pending Pod Analysis

A deployment is stuck — pods won't schedule. The agent calls `six_eyes`, which detects the Pending phase and automatically queries node pressure conditions. If nodes are under memory or disk pressure, it surfaces a `NODE_PRESSURE` finding with the specific condition, pointing the team toward cluster scaling or workload redistribution.

### Change Correlation

A pod starts crash-looping after a deployment. The agent calls `get_image_provenance` to extract the container image tag, recognises it as a Git SHA, and calls `get_commit_diff` to inspect the code change. Combined with `six_eyes`, it can correlate the crash reason (e.g. OOMKill) with a specific code change (e.g. a removed memory limit or a new unbounded cache) and point the engineer directly to the relevant diff.

### GitOps Drift Detection

An engineer asks: *"Is everything in sync?"* The agent calls `list_degraded_apps` to scan all Argo CD applications. Any app that is `OutOfSync` or `Degraded` is surfaced immediately. The agent can then call `get_app_status` on a specific app to get the full details — what repo it tracks, which revision it's targeting, and the sync error message — and suggest a remediation (manual sync, rollback, or code fix).

### Latency Investigation

A user reports that API responses are slow. The agent calls `find_slow_traces` for the `api-gateway` service with `min_duration: "2s"`. It finds several slow traces, then calls `analyze_trace` on the worst one. The report reveals that 80% of the trace duration is spent in a `db_query` span in the `payment-db` service. The agent then calls `find_pods_by_service_name` to locate the `payment-db` pods and `diagnose_pod` to check their state — completing the investigation across three domains in seconds.

### Security Forensics

A pod is behaving normally by all metrics but the team suspects compromise. The agent calls `analyze_pod_audit` on the pod. Falco reveals *"3x Terminal shell in container"* and *"1x Write below binary dir"* — someone exec'd into the container and modified a binary. The grouped summary immediately highlights the anomaly without flooding the context with hundreds of raw events.

### Log Investigation

An engineer sees a spike in 500s. The agent calls `find_slow_traces` for the `api-gateway` service, finds a trace with errors at `2024-01-15T10:32:00Z`. The agent then calls `get_log_context` with that timestamp and the `api-gateway` service to retrieve the surrounding log lines. The logs reveal a database connection timeout error right before the trace failure. The agent calls `detect_log_anomalies` on `api-gateway` and discovers that "connection timeout" errors account for 40% of recent ERROR lines — confirming a systemic issue rather than a one-off failure.

### Proactive Health Checks

An agent on a scheduled loop calls `six_eyes` across key pods in a namespace. Healthy pods return an `INFO: HEALTHY` finding and are skipped. Any pod with warnings, restarts, or throttling gets flagged for review before it becomes an incident.

## Configuration

| Environment Variable | Description | Required |
|---------------------|-------------|----------|
| `KUBECONFIG` | Path to kubeconfig file | No (falls back to `~/.kube/config` or in-cluster) |
| `PROMETHEUS_URL` | Prometheus server URL | No (Prometheus and Runes tools disabled if unset) |
| `GITHUB_TOKEN` | GitHub personal access token | No (GitHub tools disabled if unset) |
| `GITLAB_TOKEN` | GitLab personal access token | No (GitLab tools disabled if unset) |
| `GITLAB_URL` | GitLab instance URL | No (defaults to `https://gitlab.com`) |
| `ARGOCD_SERVER` | Argo CD server URL (e.g. `https://argocd.example.com`) | No (Argo CD tools disabled if unset) |
| `ARGOCD_TOKEN` | Argo CD API bearer token | No (Argo CD tools disabled if unset) |
| `JAEGER_URL` | Jaeger Query API URL (e.g. `http://jaeger-query:16686`) | No (Jaeger tools disabled if unset) |
| `FALCO_URL` | Falcosidekick URL (e.g. `http://falcosidekick:2801`) | No (Falco tools disabled if unset) |
| `LOKI_URL` | Loki API URL (e.g. `http://loki:3100`) | No (Loki tools disabled if unset) |
| `RIKUGAN_LOG_LEVEL` | Log verbosity: `debug`, `info`, `warn`, `error` | No (defaults to `info`) |

## License

See [LICENSE](LICENSE).
