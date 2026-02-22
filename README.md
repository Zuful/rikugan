# Rikugan

Rikugan is an MCP server that gives AI agents direct visibility into Kubernetes clusters and Prometheus metrics. It exposes three tools — pod diagnostics, PromQL queries, and a correlation engine — over the [Model Context Protocol](https://modelcontextprotocol.io), letting any MCP-compatible client diagnose incidents without switching between dashboards.

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

## Architecture

```
┌─────────────┐   stdin/stdout    ┌──────────────────────────────────┐
│  MCP Client  │ ◄──── JSON-RPC ────► │         Rikugan MCP Server         │
│  (AI Agent)  │                  │                                  │
└─────────────┘                  │  ┌────────────┐ ┌─────────────┐  │
                                 │  │ Kubernetes  │ │ Prometheus  │  │
                                 │  │  Provider   │ │  Provider   │  │
                                 │  └────────────┘ └─────────────┘  │
                                 │  ┌────────────────────────────┐  │
                                 │  │      Runes Provider        │  │
                                 │  │  (correlation engine)      │  │
                                 │  └────────────────────────────┘  │
                                 └──────────────────────────────────┘
                                        │                │
                                        ▼                ▼
                                 ┌────────────┐  ┌─────────────┐
                                 │ Kubernetes  │  │ Prometheus  │
                                 │   API       │  │   API       │
                                 └────────────┘  └─────────────┘
```

Providers are independent modules that register tools with a central dispatcher. The dispatcher wires each tool's schema and handler into the MCP server. Logging goes to stderr as structured JSON so it never interferes with the STDIO transport on stdout.

The correlation engine (Runes) depends on both the Kubernetes and Prometheus clients. If Prometheus is unavailable at startup, Rikugan still serves `diagnose_pod` — the Prometheus and Runes providers are simply skipped.

Core logic — query building, correlation heuristics, report generation — lives in pure functions decoupled from MCP transport. An alternative client (CLI, web, another agent framework) can import the providers directly and call `buildQueries()`, `correlate()`, or `fetchLogs()` without an MCP server.

## Getting Started

### Prerequisites

- Go 1.25+
- Access to a Kubernetes cluster (kubeconfig or in-cluster)
- Prometheus instance (optional, required for `query_range` and `six_eyes`)

### Build

```sh
go build -o rikugan ./cmd/rikugan
```

### Run

```sh
# Kubernetes only (diagnose_pod available)
./rikugan

# With Prometheus (all tools available)
PROMETHEUS_URL=http://prometheus:9090 ./rikugan
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
        "PROMETHEUS_URL": "http://localhost:9090"
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

### Proactive Health Checks

An agent on a scheduled loop calls `six_eyes` across key pods in a namespace. Healthy pods return an `INFO: HEALTHY` finding and are skipped. Any pod with warnings, restarts, or throttling gets flagged for review before it becomes an incident.

## Configuration

| Environment Variable | Description | Required |
|---------------------|-------------|----------|
| `KUBECONFIG` | Path to kubeconfig file | No (falls back to `~/.kube/config` or in-cluster) |
| `PROMETHEUS_URL` | Prometheus server URL | No (Prometheus and Runes tools disabled if unset) |

## License

See [LICENSE](LICENSE).
