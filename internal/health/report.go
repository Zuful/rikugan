package health

import (
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Status is the outcome of one signal, and Verdict the outcome of a check.
type Status string

// Signal statuses, ordered from best to worst for the evaluable ones.
const (
	StatusOK               Status = "ok"
	StatusInvestigate      Status = "investigate"
	StatusDegraded         Status = "degraded"
	StatusInsufficientData Status = "insufficient_data"
	// StatusNotEvaluated covers signals the rules skip on purpose
	// (saturation during warm-up) or whose metric does not exist.
	StatusNotEvaluated Status = "not_evaluated"
)

// Verdict is the overall result of a check. It reuses the four
// evaluable statuses.
type Verdict = Status

// Outcome says whether a check produced a verdict at all.
type Outcome string

const (
	// OutcomeEvaluated carries a verdict.
	OutcomeEvaluated Outcome = "evaluated"
	// OutcomeInProgress: the rollout has not completed, so there is no
	// "after" window yet. Instance signals of the new revision are
	// still reported.
	OutcomeInProgress Outcome = "in_progress"
	// OutcomeTooEarly: completed less than the first checkpoint ago.
	OutcomeTooEarly Outcome = "too_early"
	// OutcomeStale: completed longer than Config.StaleAfter ago.
	OutcomeStale Outcome = "stale"
)

// Signal names.
const (
	SignalRollout      = "rollout"
	SignalLatency      = "latency"
	SignalErrorRate    = "error_rate"
	SignalRestarts     = "restarts"
	SignalAvailability = "availability"
	SignalSaturation   = "saturation"
	SignalTraffic      = "traffic"
)

// SignalResult is one signal's status with the numbers behind it.
type SignalResult struct {
	Name   string
	Status Status
	Reason string
}

// Report is the result of a check.
type Report struct {
	Rollout    provider.Rollout
	Outcome    Outcome
	Verdict    Verdict       // set when Outcome is OutcomeEvaluated
	Checkpoint time.Duration // set when Outcome is OutcomeEvaluated and the rollout completed
	Signals    []SignalResult
	Notes      []string
	// NextCheck is when the first checkpoint is reached, for
	// OutcomeTooEarly.
	NextCheck time.Time
}

// Signal returns the named signal's result, if it was produced.
func (r Report) Signal(name string) (SignalResult, bool) {
	for _, s := range r.Signals {
		if s.Name == name {
			return s, true
		}
	}
	return SignalResult{}, false
}
