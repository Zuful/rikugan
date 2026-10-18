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
// Four concepts have a stable interface today: LogsProvider,
// MetricsProvider, AlertsProvider, OrchestrationProvider. Backends
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
	// Terminations holds the most recent termination of each container
	// that has one. Both Kubernetes and Docker keep only the last one;
	// older terminations are gone, which is why RestartCount, not this
	// list, is the authoritative restart signal.
	Terminations []Termination
}

// Termination describes how a container last stopped.
type Termination struct {
	Container string
	Time      time.Time
	Reason    string // OOMKilled, Killed, Error, Completed, or the runtime's own reason
	ExitCode  int
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

// Tracing, Security, Versioning and Correlation concepts will get their
// own interfaces once they have at least two implementations to keep
// honest. Until then their providers just satisfy the base Provider
// interface above.
