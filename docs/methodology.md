# Investigation Methodology

This is the playbook the agent should follow when invoked to investigate an
infrastructure incident. It is referenced from every Rikugan tool's MCP
description so any caller can read the canonical version here.

## Step 0 — What changed?

**Before looking at any signal, ask: what changed recently?** Empirically the
overwhelming majority of incidents are caused by a deploy, a config change, a
schema migration, or a dependency update — not by some spontaneous failure.
Skipping this step means you can spend an hour chasing a metric anomaly that
the deploy 20 minutes ago would have explained in seconds.

| Question | Tool |
|----------|------|
| What did we deploy recently to this service? | `argocd.get_app_status`, `argocd.list_degraded_apps` |
| What was committed in the last N hours? | `github.get_recent_commits`, `gitlab.get_recent_commits` |
| What does that commit actually change? | `github.get_commit_diff`, `gitlab.get_commit_diff` |
| Which image is the pod running right now? | `kubernetes.get_image_provenance` |
| What errors did this release introduce? | `sentry.list_issues_for_release` |

If the timeline of the incident lines up with a deploy or commit, that is your
prime suspect — pivot the rest of the investigation around verifying or
ruling it out.

## Step 1 — LEST (the Four Golden Signals)

LEST is a memorable ordering of the **Four Golden Signals** from Google SRE
(canonically Latency, Traffic, Errors, Saturation). For each letter, ask
"what does the data look like right now, and how does that compare to the
baseline before the incident started?"

### L — Latency

How long do successful requests take? Investigate degraded user experience,
timeouts, slow endpoints.

| What to check | Tool |
|---------------|------|
| Distribution of request durations across services | `jaeger.find_slow_traces` |
| Anatomy of a specific slow request (which span dominated?) | `jaeger.analyze_trace` |
| `histogram_quantile(0.95, rate(http_request_duration_seconds_bucket[5m]))` | `prometheus.query_range` |

### E — Errors

What fraction of requests fail? What is failing and why?

| What to check | Tool |
|---------------|------|
| Application error/warn logs over a window | `loki.search_logs` |
| Surrounding log context for a specific error line | `loki.get_log_context` |
| Sudden spikes in error rates compared to baseline | `loki.detect_log_anomalies` |
| `rate(http_requests_total{status=~"5.."}[5m])` | `prometheus.query_range` |
| Pod restart loops, OOMKilled, ImagePullBackOff | `kubernetes.diagnose_pod` |
| Suspicious runtime behaviour caught by Falco rules | `falco.get_security_events`, `falco.analyze_pod_audit` |
| Deduplicated application exceptions with stack traces | `sentry.list_recent_issues`, `sentry.get_issue_detail` |

### S — Saturation

How "full" is the underlying resource? CPU, memory, disk I/O, network,
connection pools, queues.

| What to check | Tool |
|---------------|------|
| `rate(container_cpu_usage_seconds_total[1m])` vs requests/limits | `prometheus.query_range` |
| `container_memory_working_set_bytes` vs limits | `prometheus.query_range` |
| Pod resource limits and actual usage | `kubernetes.diagnose_pod` |

Saturation often shows up *before* errors — a hot CPU or near-full memory is
a leading indicator. If errors are climbing, always check saturation in
parallel.

### T — Traffic

How much demand is the service receiving? Look for unusual spikes, sudden
drops, or shifts in the request mix.

| What to check | Tool |
|---------------|------|
| `rate(http_requests_total[1m])` per route | `prometheus.query_range` |
| Access log volume over the window | `loki.search_logs` |
| Cross-service request fanout patterns | `jaeger.find_slow_traces` |

A traffic anomaly (10× normal volume, sudden zero) reframes the whole
investigation — you may be looking at a runaway client, a thundering-herd
retry storm, or an outage of an upstream that stopped sending requests.

## Step 2 — Correlate

A single signal in isolation is often ambiguous. The truth lives in the
intersection: a latency spike + an error spike + a saturation climb at the
**same timestamp** is a much stronger story than any one of those alone.

| Need | Tool |
|------|------|
| Cross-signal correlation in one shot | `runes.six_eyes` |
| Map pod → service → image to align k8s and CI/CD signals | `kubernetes.find_pods_by_service`, `kubernetes.get_image_provenance` |

When you find an anomaly in one signal, immediately look at the others at the
same timestamp. Always anchor on the timestamp, not the metric — the
question is "what else was happening at 14:32:18?"

## Step 3 — Scope the blast radius

Before mitigating, know what you are dealing with:

- **One pod** → restart it, move on, investigate later
- **One service** → check the latest deploy, consider rollback
- **One cluster / one region** → operator intervention, possibly page someone
- **Cross-region** → almost certainly upstream (DNS, third-party API, shared
  dependency) — investigation pivots to "what do all affected services have
  in common?"

`kubernetes.find_pods_by_service` and `argocd.list_degraded_apps` are the
fastest ways to size the impact.

## Step 4 — Read-only first

While investigating, **do not** trigger rollbacks, restarts, or scale-changes
unless the impact is immediately catastrophic. Read-only investigation does
not change the state of the system; mitigation might destroy the very
evidence you need to find the root cause (e.g. rolling a pod loses its
in-memory state, restarting clears logs that were not yet shipped).

Form a hypothesis, validate it, *then* mitigate.

## Complementary frameworks

LEST is the primary lens, but two adjacent frameworks are worth knowing for
specific contexts:

### RED — Rate, Errors, Duration

Simpler subset of LEST that drops Saturation. Best for **request-driven
services** (APIs, microservices). Maps cleanly to LEST: R=T, E=E, D=L.
Useful when you want to keep dashboards minimal.

### USE — Utilization, Saturation, Errors

Brendan Gregg's method, focused on **physical resources** (CPU, memory,
disk, network) rather than services. Complements LEST: where LEST looks at
the service, USE looks at the box it runs on.

Use LEST as the default. Reach for RED when the surface is a clean API,
USE when the suspect is a hardware/resource constraint rather than a
service-level concern.

## Quick reference

| Step | Question | Primary tools |
|------|----------|---------------|
| 0 | What changed? | argocd, github, gitlab, sentry (release attribution) |
| L | How slow is it? | jaeger, prometheus |
| E | What is failing? | loki, prometheus, kubernetes, falco, sentry |
| S | Are we out of headroom? | prometheus, kubernetes |
| T | Is the request volume normal? | prometheus, loki |
| Correlate | Same timestamp, multiple signals | runes |
| Scope | How wide is the impact? | kubernetes, argocd |
| Mitigate | Only after hypothesis | (out of scope for read-only investigation) |
