package health

import "fmt"

// Backend kinds a check reads.
const (
	BackendMetrics       = "metrics"
	BackendOrchestration = "orchestration"
)

// BackendError is a check that could not read one of its backends. With
// Kind BackendMetrics, the check still returns its report: the instance
// signals were read, and only the metrics-based signals are missing.
type BackendError struct {
	Kind string // BackendMetrics or BackendOrchestration
	Name string // the provider, such as prometheus or kubernetes
	Err  error
}

func (e *BackendError) Error() string { return fmt.Sprintf("%s backend %s: %v", e.Kind, e.Name, e.Err) }

func (e *BackendError) Unwrap() error { return e.Err }

// metricsUnavailable marks every metrics-based signal of r as not
// evaluated, replaces the per-query notes with one, and recomputes the
// verdict from what is left: the instance signals.
func metricsUnavailable(r *Report, name string, err error) {
	reason := fmt.Sprintf("metrics backend %s failing", name)
	for i, s := range r.Signals {
		switch s.Name {
		case SignalLatency, SignalErrorRate, SignalSaturation, SignalTraffic, SignalMemoryLeak:
			r.Signals[i].Status, r.Signals[i].Reason = StatusNotEvaluated, reason
		}
	}
	r.Notes = append(r.Notes, fmt.Sprintf("metrics not evaluated: %s: %v", reason, err))
	if r.Outcome == OutcomeEvaluated && r.Verdict != "" {
		r.Verdict = overall(r.Signals)
	}
}
