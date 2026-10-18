package provider

import (
	"context"
	"time"
)

// The interfaces in this file are the canonical per-concept contracts.
// Providers translate their backend's native API into these shared
// types so each concept's tool registry (under internal/<concept>/)
// can expose technology-agnostic tool names — get_logs,
// list_active_alerts, get_service_state — that route to whichever
// provider the operator configured.
//
// Six concepts have an interface today: LogsProvider,
// MetricsProvider, AlertsProvider, OrchestrationProvider,
// DeploymentSource and CommunicationProvider. Backends
// that implement them also satisfy the base Provider (Name + Tools)
// contract above so they can register backend-specific tools that
// don't project into the strict-intersection interface (e.g. loki's
// detect_log_anomalies, pagerduty's get_current_oncalls). The
// versioning, tracing, and security concepts keep the base Provider
// contract only — their interface will land once a second backend
// makes the abstraction honest.

// Service is the orchestration-layer abstraction over a "deployable
// unit" (a Kubernetes Service + its Pods, a Docker Compose service
// definition, etc.). Provider-specific identifiers live in Labels.
type Service struct {
	Name      string
	Namespace string
	Kind      string // pod, deployment, statefulset, docker-compose-service, …
	Labels    map[string]string
}

// ServiceState is a snapshot of a Service's runtime health.
type ServiceState struct {
	Status   string // running, degraded, failed, pending, unknown
	Replicas struct {
		Desired   int
		Available int
	}
	Message string
}

// Event is a noteworthy occurrence attached to a Service (restart,
// scheduling failure, OOMKill, image pull error, …).
type Event struct {
	Time    time.Time
	Type    string // Normal, Warning
	Reason  string
	Message string
}

// OrchestrationProvider exposes operational-shape introspection over a
// service runtime. Kubernetes and Docker Compose are the two reference
// implementations; the interface is meant to be the strict intersection
// of what both can express.
type OrchestrationProvider interface {
	Provider
	ListServices(ctx context.Context, namespace string) ([]Service, error)
	GetServiceState(ctx context.Context, svc Service) (ServiceState, error)
	DescribeService(ctx context.Context, svc Service) (string, error)
	GetRecentEvents(ctx context.Context, svc Service, since time.Duration) ([]Event, error)
	RestartService(ctx context.Context, svc Service) error
	GetServiceHistory(ctx context.Context, svc Service) (string, error)
	// GetRevisionInstances returns the live instances (pods, containers)
	// of one revision of the service. revision is the backend's identity
	// for a pod/container template — the Kubernetes pod-template-hash,
	// the Compose config-hash — and an empty revision means the one the
	// service currently runs. Callers that evaluate a rollout's health
	// read restarts, terminations and readiness from here rather than
	// from a backend client.
	GetRevisionInstances(ctx context.Context, svc Service, revision string) ([]Instance, error)
}

// Instance is one running unit of a service revision: a Kubernetes pod
// or a Docker Compose container.
type Instance struct {
	Name     string
	Revision string
	Created  time.Time
	Ready    bool
	// NotReadySince is when the instance last stopped being ready, or
	// when it was created if it has never been ready. Zero when Ready.
	NotReadySince time.Time
	// RestartCount is the runtime's restart counter, summed over the
	// instance's containers. It counts restarts since the instance was
	// created, so for the instances of a fresh revision it counts the
	// restarts since the rollout.
	RestartCount int
	CrashLooping bool
	// Terminations holds the terminations the runtime still knows
	// about, oldest first: the last one per container on Kubernetes,
	// possibly several on Docker (read from its event log). Older ones
	// may be gone, which is why RestartCount, not this list, is the
	// authoritative restart signal.
	Terminations []Termination
}

// Termination describes how a container last stopped.
type Termination struct {
	Container string
	Time      time.Time
	Reason    string // OOMKilled, Killed, Error, Completed, Unknown, or the runtime's own reason
	ExitCode  int    // -1 when the runtime no longer knows it (Reason Unknown)
	// OOM is true only when the runtime attests it (Kubernetes reason
	// OOMKilled, Docker State.OOMKilled). Exit code 137 alone means
	// SIGKILL — also what docker stop/kill and eviction produce — and
	// is reported as Reason "Killed" with OOM false.
	OOM bool
}

// LogEntry is a single log line with the labels that scoped its stream.
type LogEntry struct {
	Time    time.Time
	Labels  map[string]string
	Message string
}

// LogQuery scopes a log query — at least one of Service / Namespace /
// Labels is expected; Match is a free-form substring or LogQL/native
// query (the provider decides which dialect it accepts).
type LogQuery struct {
	Service   string
	Namespace string
	Labels    map[string]string
	Match     string
	Start     time.Time
	End       time.Time
	Limit     int
}

// LogsProvider exposes operational-shape log retrieval. Loki, kubectl
// logs, Docker logs are the reference implementations.
type LogsProvider interface {
	Provider
	GetLogs(ctx context.Context, q LogQuery) ([]LogEntry, error)
	TailLogs(ctx context.Context, q LogQuery, lines int) ([]LogEntry, error)
	SearchLogs(ctx context.Context, q LogQuery) ([]LogEntry, error)
}

// MetricSample is one (timestamp, value) pair on a labelled series.
type MetricSample struct {
	Time   time.Time
	Value  float64
	Labels map[string]string
}

// MetricsProvider exposes operational-shape metric retrieval.
// Prometheus and kubectl top are the reference implementations.
type MetricsProvider interface {
	Provider
	GetMetric(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]MetricSample, error)
	CompareToBaseline(ctx context.Context, query string, window, baseline time.Duration) (string, error)
	ListAvailableMetrics(ctx context.Context, matchPrefix string) ([]string, error)
}

// Alert is the cross-provider shape of an actionable runtime signal.
// AlertManager firings, Sentry issues and PagerDuty incidents all
// project into this shape with provider-specific extras in Extras.
type Alert struct {
	ID          string
	Title       string
	Description string
	Severity    string // info, warning, error, critical
	State       string // firing, acknowledged, resolved, silenced
	StartedAt   time.Time
	Service     string
	Source      string         // alertmanager, sentry, pagerduty, …
	Extras      map[string]any // provider-specific fields preserved verbatim
}

// AlertsProvider exposes the strict intersection of incident- and
// error-management read operations. AlertManager (firings),
// Sentry (issues), PagerDuty (incidents) are the reference
// implementations. Mutating operations (ack/resolve/silence) don't
// intersect cleanly across the three — AlertManager has no per-alert
// ack, Sentry has resolve but no ack, PagerDuty has all three with
// different semantics — so they stay as backend-specific tools rather
// than being forced into the concept interface. The shared
// `service string` argument is the operator-chosen scoping label;
// each backend maps it to its native concept (AM label, Sentry
// project, PagerDuty service-by-name).
type AlertsProvider interface {
	Provider
	ListActiveAlerts(ctx context.Context, service string) ([]Alert, error)
	GetAlertDetails(ctx context.Context, alertID string) (Alert, error)
}

// RolloutState is where a rollout stands.
type RolloutState string

// Rollout states.
const (
	// RolloutInProgress means the new revision is not fully available
	// yet and the backend is still waiting for it.
	RolloutInProgress RolloutState = "in_progress"
	// RolloutComplete means every instance runs the new revision and is
	// available.
	RolloutComplete RolloutState = "complete"
	// RolloutFailed means the backend has given up on the rollout, or
	// it has exceeded its deadline: the new revision will not become
	// available without intervention.
	RolloutFailed RolloutState = "failed"
	// RolloutSuperseded means a newer rollout of the same service
	// started before this one completed or failed. Only WatchRollouts
	// reports it: the current rollout is never superseded.
	RolloutSuperseded RolloutState = "superseded"
)

// Rollout is one change of the revision a service runs, as seen by the
// runtime or delivery system that performs it.
type Rollout struct {
	Service Service
	// Revision is the backend's identity for the rolled-out template
	// (Kubernetes pod-template-hash, Compose config-hash). It is the
	// value OrchestrationProvider.GetRevisionInstances takes.
	Revision string
	// Sequence is the human-facing revision number when the backend
	// has one (the Kubernetes deployment.kubernetes.io/revision), empty
	// otherwise.
	Sequence string
	Images   []string
	Rollback bool
	State    RolloutState
	// StartedAt ends the "before" comparison window.
	StartedAt time.Time
	// CompletedAt starts the "after" comparison window. Zero unless
	// State is RolloutComplete.
	CompletedAt time.Time
	// FailedAt and FailureReason are set when State is RolloutFailed.
	FailedAt      time.Time
	FailureReason string
}

// DeploymentSource reports the rollouts of services (concept 5b in
// docs/ontology.md: what runs in production and since when, as opposed
// to versioning's what changed in the code). It ships with a single
// backend, kubernetes; docs/ontology.md records why that exception to
// the two-backend rule is accepted. dockercompose joins it next.
type DeploymentSource interface {
	Provider
	// LastRollout returns the service's current rollout: the one that
	// produced the revision it runs or is moving to.
	LastRollout(ctx context.Context, svc Service) (Rollout, error)
	// WatchRollouts reports rollouts as they end: completed, failed, or
	// superseded by a newer one. A rollout that fails and later
	// completes is reported twice, once per state. At start, the
	// current rollout of every watched service is reported if it has
	// already ended, so a consumer deduplicates on (service, sequence,
	// state) against what it already recorded. The channel is closed
	// when ctx is done.
	WatchRollouts(ctx context.Context) (<-chan Rollout, error)
}

// Severity is how urgently a team message needs a human. It is generic
// on purpose: the communication concept knows nothing of deploy-health
// verdicts, whose mapping to a severity lives with the verdicts.
type Severity string

// Severities, from least to most urgent.
const (
	// SeverityInfo is for the record: nobody needs to act.
	SeverityInfo Severity = "info"
	// SeverityWarning asks someone to look when they can.
	SeverityWarning Severity = "warning"
	// SeverityCritical asks someone to look now.
	SeverityCritical Severity = "critical"
)

// Link is a labelled URL attached to a team message.
type Link struct {
	Text string
	URL  string
}

// TeamMessage is a message for the humans of a team. Body is plain
// text; each provider escapes and formats it for its own channel.
type TeamMessage struct {
	Severity Severity
	Title    string
	Body     string
	Links    []Link
}

// CommunicationProvider posts messages to a team channel (concept 7 in
// docs/ontology.md). Its backends are telegram and slack.
type CommunicationProvider interface {
	Provider
	PostMessage(ctx context.Context, msg TeamMessage) error
}

// Tracing, Security, Versioning and Correlation concepts will get their
// own interfaces once they have at least two implementations to keep
// honest. Until then their providers just satisfy the base Provider
// interface above.
