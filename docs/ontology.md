# Ontology

This is the list of operational concepts Rikugan models. A concept is an
operational need — "what are the logs?", "what is running?", "who do I
tell?" — independent of the technology that answers it. Tools are named
after the concept (`get_logs`, `list_active_alerts`), and backends plug in
behind a per-concept interface in [`pkg/provider`](../pkg/provider).

Priorities say which concepts a stack must cover for Rikugan to be useful
(P0), which ones make it substantially better (P1), and which ones are
planned but not yet on the critical path (P2).

## Concepts

| # | Concept | Package name | Priority | Interface in `pkg/provider` | Backends today | Planned |
|---|---|---|---|---|---|---|
| 1 | Orchestration | `orchestration` | P0 | `OrchestrationProvider` | kubernetes, dockercompose | — |
| 2 | Logs | `logs` | P0 | `LogsProvider` | loki | — |
| 3 | Metrics | `metrics` | P0 | `MetricsProvider` | prometheus | newrelic (#6) |
| 4 | Alerts | `alerts` | P0 | `AlertsProvider` | alertmanager, sentry, pagerduty | — |
| 5a | Versioning — commits, diffs | `versioning` | P1 | none yet | github, gitlab | — |
| 5b | Deployments — rollouts, deployed revisions | `deployments` | P1 | `DeploymentSource` | kubernetes | dockercompose (proactive mode phase 2), argocd (phase 3) |
| 6 | Incidents and tickets | `incidents` | P1 | none yet | none as a concept — see below | — |
| 7 | Team communication | `communication` | P1 | `CommunicationProvider` *(planned, proactive mode phase 2)* | none yet | telegram, slack (phase 2) |
| 8 | CI/CD | `cicd` | P2 | none yet | none | — |
| 9 | Distributed tracing | `tracing` | P2 | none yet | jaeger | — |
| 10 | Databases | `databases` | P2 | none yet | none | — |

"Backends today" lists what is wired into the code at the time of
writing. When a concept has no interface yet, its backends register
technology-specific tools directly on the dispatcher.

## Where today's code sits relative to the ontology

The ontology is ahead of the package layout in a few places. Nothing has
been moved yet; these are the known gaps.

- **`internal/versioning/argocd`** (`get_app_status`, `list_degraded_apps`)
  answers "what is deployed and is it healthy?" — that is concept 5b,
  not 5a. It moves to `internal/deployments/argocd` when the argocd
  `DeploymentSource` provider lands (proactive mode phase 3).
- **`internal/versioning/kubernetes`** (`get_image_provenance`) maps a
  running image back to a commit. It is the bridge between 5b and 5a:
  it reads deployment state and answers in versioning terms. It stays in
  `versioning` for now; moving it to `deployments`, which now exists, is
  a pending follow-up.
- **PagerDuty** lives under `alerts` and implements `AlertsProvider`.
  Its incident timeline and `get_current_oncalls` are concept 6 material;
  they stay in `alerts` until a second incident/ticketing backend
  justifies an `incidents` interface.
- **Security** (`internal/security/falco`: `get_security_events`,
  `analyze_pod_audit`) is not in the list above. *Open question:* add it
  as its own concept, or treat runtime security events as a kind of
  alert?
- **Correlation** (`internal/correlation/runes`: `six_eyes`) is not a
  concept. It is a cross-concept layer that combines several concepts to
  answer one question, and by design never has a backend interface of
  its own. `check_deploy_health` (`internal/correlation/deployhealth`)
  belongs here too: it reads deployments, orchestration and metrics
  through their interfaces, and its rules live in `internal/health`.

## Why "versioning and deployments" was split in two

The original catalogue had a single concept 5, *Versioning and
deployments*. Designing the proactive mode (issue #4) showed that it
mixed two different questions:

- **Versioning (5a):** *what changed in the code?* Commits, diffs, merge
  requests. The source of truth is the forge (GitHub, GitLab), and the
  timeline is the commit history.
- **Deployments (5b):** *what is running in production, and since when?*
  Rollouts, the revision currently serving traffic, when a rollout
  started and finished. The source of truth is the runtime or the
  delivery system (Kubernetes, Argo CD, Docker Compose), and the
  timeline is the rollout history.

The two timelines do not line up. A commit can sit undeployed for days, a
rollback deploys an old commit, and one commit can be rolled out at
different times in different environments. The proactive mode needs the
second timeline only: its "after" window starts when a rollout
*completes*, which no forge can tell you. Keeping both in one concept
would have forced a single interface to answer both, with every backend
implementing half of it.

## The "second backend" rule, and where `deployments` breaks it

Rikugan's rule is that a concept gets an interface in `pkg/provider` once
a second backend makes the abstraction honest. One backend alone tends
to produce an interface shaped like that backend's API.

`deployments` breaks this rule on purpose, for one phase. It got its
interface in proactive mode phase 1 (#34) with kubernetes as its only
backend; dockercompose joins in phase 2 and argocd in phase 3. This is
accepted because:

- the proactive mode's rules engine must depend on a concept interface,
  not on Kubernetes, from day one. Otherwise it would have to be
  rewritten when the second backend lands;
- the second backend (dockercompose) is planned for the very next
  phase, with a known shape (a new `com.docker.compose.config-hash` on
  the service's containers, complete once they are all ready), so the
  interface can be checked against it on paper now;
- the interface is kept minimal (`LastRollout`, then `WatchRollouts`) to
  limit how much of Kubernetes can leak into it.

If dockercompose does not fit when it lands, the interface changes.
Being wrong here costs one refactor of a small, internal interface.

`communication` gets its interface in phase 2 with two backends
(telegram, slack) arriving together, so it follows the rule.
