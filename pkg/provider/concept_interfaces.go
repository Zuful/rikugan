package provider

import (
	"context"
	"time"
)

// The interfaces in this file are the **draft** per-concept contracts
// for Étape 3 of the refactor. They are intentionally not yet
// implemented by any provider — Étape 2 only declares them so the
// concept folders have a target to converge on. Étape 3 will finalise
// the method shapes (likely by tightening them based on what each
// provider can actually express) and then refactor each provider to
// implement them. See REFACTOR_PLAN.md for the full plan.
//
// Until Étape 3 lands, every provider continues to satisfy the
// underlying `Provider` (Name + Tools) contract above, and tool
// registration goes through the dispatcher unchanged.

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

// AlertsProvider exposes incident- and error-management operations.
// AlertManager (firings), Sentry (errors), PagerDuty (incidents) are
// the reference implementations; provider-specific tools that can't
// project into this interface (e.g. PagerDuty on-call rotations) stay
// outside the interface as concept-level optional tools.
type AlertsProvider interface {
	Provider
	ListActiveAlerts(ctx context.Context, service string) ([]Alert, error)
	GetAlertDetails(ctx context.Context, alertID string) (Alert, error)
	AckAlert(ctx context.Context, alertID, by string) error
	ResolveAlert(ctx context.Context, alertID, by string) error
	SilenceAlert(ctx context.Context, alertID string, until time.Time, comment string) error
}

// Tracing, Security, Versioning and Correlation concepts will get their
// own interfaces once they have at least two implementations to keep
// honest. Until then their providers just satisfy the base Provider
// interface above.
