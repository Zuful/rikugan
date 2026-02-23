# Rikugan

Rikugan is an MCP server that gives AI agents direct visibility into Kubernetes clusters, Prometheus metrics, code change history from GitHub and GitLab, Argo CD deployment state, distributed traces from Jaeger, and runtime security events from Falco. It exposes fifteen tools — pod diagnostics, cross-domain mapping, PromQL queries, a correlation engine, change intelligence, deployment status, trace analysis, and security audit — over the [Model Context Protocol](https://modelcontextprotocol.io), letting any MCP-compatible client diagnose incidents end-to-end without switching between dashboards.

## What is MCP?

The Model Context Protocol is an open standard for connecting AI models to external tools and data sources. Instead of embedding API calls in prompts, MCP defines a structured interface where a server advertises tools with typed schemas, and a client (typically an AI agent) invokes them. Rikugan implements MCP over STDIO transport — it reads JSON-RPC from stdin and writes responses to stdout, making it easy to integrate with any MCP client.

## Tools

### `diagnose_pod`

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

### `find_pods_by_service_name`

Bridges Jaeger service names to Kubernetes pod names. Finds pods that belong to a logical service.

| Parameter      | Type   | Required | Description                                        |
|----------------|--------|----------|----------------------------------------------------|
| `service_name` | string | yes      | Logical service name (e.g. from a Jaeger trace)    |
| `namespace`    | string | no       | Kubernetes namespace (default: all namespaces)      |

Searches by common labels (`app`, `app.kubernetes.io/name`) first, then falls back to pod name prefix matching if no labels match. The report indicates which match method was used so the LLM knows the confidence level, and suggests next steps (`diagnose_pod` or `six_eyes`).

### `get_image_provenance`

Bridges running pods to source code by extracting container image tags and identifying Git SHAs.

| Parameter   | Type   | Required | Description                     |
|-------------|--------|----------|---------------------------------|
| `namespace` | string | yes      | Kubernetes namespace            |
| `pod_name`  | string | yes      | Name of the pod to inspect      |

Parses each container's image reference into registry, repository, and tag. If a tag matches a Git SHA pattern (7-40 hex characters), it's flagged and the report includes an explicit suggestion: *"To see the code: use `get_commit_diff` with `commit_sha: a1b2c3d`"*. Covers both regular and init containers.

### `query_range`

Executes a PromQL range query against Prometheus and returns formatted time-series data.

| Parameter | Type   | Required | Description                                  |
|-----------|--------|----------|----------------------------------------------|
| `query`   | string | yes      | PromQL expression                            |
| `start`   | string | no       | RFC 3339 timestamp (default: 1 hour ago)     |
| `end`     | string | no       | RFC 3339 timestamp (default: now)            |
| `step`    | string | no       | Step duration, e.g. `60s`, `5m` (default: `60s`) |

A data-point guard rejects queries that would return more than 10,000 samples, preventing excessive data transfer to the agent.

### `six_eyes`

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

### `get_recent_commits`

Fetches the most recent commits from a GitHub repository. Useful for correlating code changes with runtime incidents.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `owner`   | string | yes      | GitHub repository owner (user or organisation)   |
| `repo`    | string | yes      | GitHub repository name                           |
| `branch`  | string | no       | Branch name (default: `main`)                    |

Returns a Markdown table with the last 10 commits showing short SHA, author, date, and first line of the commit message.

### `get_commit_diff`

Fetches the full diff for a specific commit, including per-file patches.

| Parameter    | Type   | Required | Description                                    |
|--------------|--------|----------|------------------------------------------------|
| `owner`      | string | yes      | GitHub repository owner (user or organisation) |
| `repo`       | string | yes      | GitHub repository name                         |
| `commit_sha` | string | yes      | Full or abbreviated commit SHA                 |

Returns commit metadata (SHA, author, date, message) followed by per-file diffs. Output is capped at 10,000 characters to avoid overwhelming the agent context — if the diff exceeds this limit it is truncated with a note indicating the total size.

### `gitlab_get_recent_commits`

Fetches the most recent commits from a GitLab project. Equivalent to `get_recent_commits` for GitLab-hosted repositories, including self-hosted instances.

| Parameter    | Type   | Required | Description                                                        |
|--------------|--------|----------|--------------------------------------------------------------------|
| `project_id` | string | yes      | GitLab project ID (numeric) or URL-encoded path (e.g. `group/project`) |
| `branch`     | string | no       | Branch name (default: `main`)                                      |

Returns a Markdown table with the last 10 commits showing short SHA, author, date, and commit title.

### `gitlab_get_commit_diff`

Fetches the full diff for a specific commit from a GitLab project.

| Parameter    | Type   | Required | Description                                                        |
|--------------|--------|----------|--------------------------------------------------------------------|
| `project_id` | string | yes      | GitLab project ID (numeric) or URL-encoded path (e.g. `group/project`) |
| `commit_sha` | string | yes      | Full or abbreviated commit SHA                                     |

Returns commit metadata followed by per-file diffs. Same 10,000-character truncation as the GitHub equivalent.

### `get_app_status`

Gets the sync and health status of an Argo CD application.

| Parameter  | Type   | Required | Description                      |
|------------|--------|----------|----------------------------------|
| `app_name` | string | yes      | Name of the Argo CD application  |

Returns a report with sync status (`Synced`, `OutOfSync`), health status (`Healthy`, `Degraded`, `Missing`), tracked repository, target revision, and the last operation details including any error messages.

### `list_degraded_apps`

Lists all Argo CD applications that are currently Degraded, Missing, or OutOfSync.

| Parameter | Type   | Required | Description                                          |
|-----------|--------|----------|------------------------------------------------------|
| `project` | string | no       | Argo CD project name to filter by (default: all)     |

Returns a summary table of unhealthy applications with their sync status, health status, repository, and error messages. Useful for a quick health check across all GitOps deployments.

### `find_slow_traces`

Finds the most recent slow traces for a service from Jaeger.

| Parameter      | Type   | Required | Description                                                     |
|----------------|--------|----------|-----------------------------------------------------------------|
| `service`      | string | yes      | Service name to search traces for                               |
| `min_duration` | string | no       | Minimum trace duration (e.g. `500ms`, `1s`, `2.5s`). Default: no minimum |

Returns a summary table of up to 5 traces with trace ID, start time, total duration, and span count. Searches a 24-hour lookback window.

### `analyze_trace`

Analyzes a Jaeger trace and returns a synthesized bottleneck report instead of raw span JSON.

| Parameter  | Type   | Required | Description              |
|------------|--------|----------|--------------------------|
| `trace_id` | string | yes      | Jaeger trace ID to analyze |

Parses all spans in the trace, identifies the 3 slowest spans and any spans with errors, and returns a structured report including:
- Total trace duration and span count
- A natural-language bottleneck summary (e.g. *"The bottleneck was `db_query` (1.8s) in service `payment-db`"*)
- Ranked table of slowest spans with service, operation, duration, and percentage of total time
- Error table listing any spans with error tags or log entries

### `get_security_events`

Fetches recent Falco security events from Falcosidekick with optional filtering.

| Parameter   | Type   | Required | Description                                                          |
|-------------|--------|----------|----------------------------------------------------------------------|
| `namespace` | string | no       | Kubernetes namespace to filter events by                             |
| `pod_name`  | string | no       | Pod name to filter events by                                         |
| `severity`  | string | no       | Minimum severity: `Notice`, `Warning`, `Error`, `Critical` (default: `Warning`) |

Returns a table of up to 50 events from the last 24 hours showing time, severity, rule name, and output message. Server-side filtering by priority, client-side filtering by namespace/pod.

### `analyze_pod_audit`

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

## Architecture

```
┌─────────────┐   stdin/stdout    ┌──────────────────────────────────────────┐
│  MCP Client  │ ◄──── JSON-RPC ────► │            Rikugan MCP Server            │
│  (AI Agent)  │                  │                                          │
└─────────────┘                  │  ┌──────────┐ ┌──────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐│
                                 │  │Kubernetes│ │Prometheus│ │ GitHub │ │ GitLab │ │Argo CD │ │ Jaeger │ │ Falco  ││
                                 │  │ Provider │ │ Provider │ │Provider│ │Provider│ │Provider│ │Provider│ │Provider││
                                 │  └──────────┘ └──────────┘ └────────┘ └────────┘ └────────┘ └────────┘ └────────┘│
                                 │  ┌──────────────────────────────────────┐│
                                 │  │          Runes Provider               ││
                                 │  │       (correlation engine)            ││
                                 │  └──────────────────────────────────────┘│
                                 └──────────────────────────────────────────┘
                                        │                │              │
                                        ▼                ▼              ▼
                                 ┌──────────┐ ┌──────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌────────┐ ┌──────────────┐
                                 │Kubernetes│ │Prometheus│ │ GitHub │ │ GitLab │ │Argo CD │ │ Jaeger │ │Falcosidekick│
                                 │  API     │ │  API     │ │  API   │ │  API   │ │  API   │ │  API   │ │     API      │
                                 └──────────┘ └──────────┘ └────────┘ └────────┘ └────────┘ └────────┘ └──────────────┘
```

Providers are independent modules that register tools with a central dispatcher. The dispatcher wires each tool's schema and handler into the MCP server. Logging goes to stderr as structured JSON so it never interferes with the STDIO transport on stdout.

The correlation engine (Runes) depends on both the Kubernetes and Prometheus clients. If Prometheus is unavailable at startup, Rikugan still serves `diagnose_pod` — the Prometheus and Runes providers are simply skipped. The GitHub, GitLab, Argo CD, Jaeger, and Falco providers are independently optional — if their respective configuration is unset, they are skipped with a warning and the remaining providers continue to function. Multiple providers can be active simultaneously; the AI agent chooses the appropriate tool based on context.

Core logic — query building, correlation heuristics, report generation — lives in pure functions decoupled from MCP transport. An alternative client (CLI, web, another agent framework) can import the providers directly and call `buildQueries()`, `correlate()`, or `fetchLogs()` without an MCP server.

## Getting Started

### Prerequisites

- Go 1.25+
- Access to a Kubernetes cluster (kubeconfig or in-cluster)
- Prometheus instance (optional, required for `query_range` and `six_eyes`)
- GitHub personal access token (optional, required for `get_recent_commits` and `get_commit_diff`)
- GitLab personal access token (optional, required for `gitlab_get_recent_commits` and `gitlab_get_commit_diff`)
- Argo CD server URL and API token (optional, required for `get_app_status` and `list_degraded_apps`)
- Jaeger Query API URL (optional, required for `find_slow_traces` and `analyze_trace`)
- Falcosidekick URL (optional, required for `get_security_events` and `analyze_pod_audit`)

### Build

```sh
go build -o rikugan ./cmd/rikugan
```

### Run

```sh
# Kubernetes only (diagnose_pod available)
./rikugan

# With Prometheus (query_range and six_eyes available)
PROMETHEUS_URL=http://prometheus:9090 ./rikugan

# With GitHub (change intelligence available)
GITHUB_TOKEN=ghp_... ./rikugan

# With GitLab (self-hosted instance)
GITLAB_TOKEN=glpat-... GITLAB_URL=https://gitlab.example.com ./rikugan

# With Argo CD (deployment state)
ARGOCD_SERVER=https://argocd.example.com ARGOCD_TOKEN=eyJ... ./rikugan

# With Jaeger (trace analysis)
JAEGER_URL=http://jaeger-query:16686 ./rikugan

# With Falco (runtime security audit)
FALCO_URL=http://falcosidekick:2801 ./rikugan

# All providers
PROMETHEUS_URL=http://prometheus:9090 GITHUB_TOKEN=ghp_... GITLAB_TOKEN=glpat-... \
  ARGOCD_SERVER=https://argocd.example.com ARGOCD_TOKEN=eyJ... \
  JAEGER_URL=http://jaeger-query:16686 FALCO_URL=http://falcosidekick:2801 ./rikugan
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
        "FALCO_URL": "http://falcosidekick:2801"
      }
    }
  }
}
```

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

A pod starts crash-looping after a deployment. The agent calls `get_recent_commits` (or `gitlab_get_recent_commits`) on the service's repository to see what changed, then uses the corresponding diff tool to inspect the suspicious commit. Combined with `six_eyes`, it can correlate the crash reason (e.g. OOMKill) with a specific code change (e.g. a removed memory limit or a new unbounded cache) and point the engineer directly to the relevant diff.

### GitOps Drift Detection

An engineer asks: *"Is everything in sync?"* The agent calls `list_degraded_apps` to scan all Argo CD applications. Any app that is `OutOfSync` or `Degraded` is surfaced immediately. The agent can then call `get_app_status` on a specific app to get the full details — what repo it tracks, which revision it's targeting, and the sync error message — and suggest a remediation (manual sync, rollback, or code fix).

### Latency Investigation

A user reports that API responses are slow. The agent calls `find_slow_traces` for the `api-gateway` service with `min_duration: "2s"`. It finds several slow traces, then calls `analyze_trace` on the worst one. The report reveals that 80% of the trace duration is spent in a `db_query` span in the `payment-db` service, immediately pointing the team to a database query that needs optimization.

### Security Forensics

A pod is behaving normally by all metrics but the team suspects compromise. The agent calls `analyze_pod_audit` on the pod. Falco reveals *"3x Terminal shell in container"* and *"1x Write below binary dir"* — someone exec'd into the container and modified a binary. The grouped summary immediately highlights the anomaly without flooding the context with hundreds of raw events.

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
| `RIKUGAN_LOG_LEVEL` | Log verbosity: `debug`, `info`, `warn`, `error` | No (defaults to `info`) |

## License

See [LICENSE](LICENSE).
