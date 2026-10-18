// Package sweep holds the per-service state machine of the periodic
// sweep (design §6): given a service's state and the verdict of a new
// sweep round, it returns the next state and whether to notify. It is
// pure; the daemon persists State in the journal and sends the
// notifications.
package sweep

import (
	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

// Phase is where a service stands between sweep rounds.
type Phase string

// Phases.
const (
	// PhaseOK is the resting phase.
	PhaseOK Phase = "ok"
	// PhaseWarning counts consecutive "investigate" rounds.
	PhaseWarning Phase = "warning"
	// PhaseIncident follows a "degraded" round.
	PhaseIncident Phase = "incident"
	// PhaseResolving counts OK rounds after an incident.
	PhaseResolving Phase = "resolving"
)

// State is a service's sweep state. The zero value is PhaseOK.
type State struct {
	Phase Phase `json:"phase,omitempty"`
	// Streak is the number of consecutive "investigate" rounds in
	// PhaseWarning, and of consecutive OK rounds in PhaseResolving.
	Streak int `json:"streak,omitempty"`
	// WarningNotified records that the persistent-warning notification
	// went out for the current warning series.
	WarningNotified bool `json:"warning_notified,omitempty"`
}

func (s State) phase() Phase {
	if s.Phase == "" {
		return PhaseOK
	}
	return s.Phase
}

// Defaults for Config.
const (
	DefaultWarningPersistence = 3
	// okRoundsToResolve is fixed by the design: two consecutive OK
	// rounds absorb oscillation.
	okRoundsToResolve = 2
)

// Config holds the sweep's notification settings.
type Config struct {
	// NotifyWarning (sweep_notify_warning) notifies every entry into the
	// warning phase. Off by default: an isolated "investigate" outside
	// any change is more often noise.
	NotifyWarning bool
	// WarningPersistence (sweep_warning_persistence) is the number of
	// consecutive "investigate" rounds that triggers one warning
	// notification. Zero means DefaultWarningPersistence.
	WarningPersistence int
}

func (c Config) persistence() int {
	if c.WarningPersistence <= 0 {
		return DefaultWarningPersistence
	}
	return c.WarningPersistence
}

// Event names what a notification is about.
type Event string

// Events.
const (
	// EventIncident: the service entered an incident.
	EventIncident Event = "incident"
	// EventWarning: the service entered the warning phase, notified
	// only with Config.NotifyWarning.
	EventWarning Event = "warning"
	// EventPersistentWarning: "investigate" for WarningPersistence
	// rounds in a row.
	EventPersistentWarning Event = "persistent_warning"
	// EventResolved: two consecutive OK rounds after an incident.
	EventResolved Event = "resolved"
)

// Notification is what the daemon should send after a round. Streak is
// the number of rounds behind a persistent warning.
type Notification struct {
	Event    Event
	Severity provider.Severity
	Streak   int
}

// Step applies one sweep round's verdict to s. The returned
// notification is nil when nothing is to be sent.
//
// An "insufficient data" round never moves the state: no progress
// towards resolution, and a warning series neither advances nor breaks.
func Step(s State, verdict health.Verdict, cfg Config) (State, *Notification) {
	if verdict == health.StatusInsufficientData || verdict == health.StatusNotEvaluated {
		return s, nil
	}
	switch s.phase() {
	case PhaseOK:
		switch verdict {
		case health.StatusDegraded:
			return State{Phase: PhaseIncident}, incident()
		case health.StatusInvestigate:
			return enterWarning(cfg)
		}
		return State{}, nil

	case PhaseWarning:
		switch verdict {
		case health.StatusDegraded:
			return State{Phase: PhaseIncident}, incident()
		case health.StatusInvestigate:
			next := State{Phase: PhaseWarning, Streak: s.Streak + 1, WarningNotified: s.WarningNotified}
			if !next.WarningNotified && next.Streak >= cfg.persistence() {
				next.WarningNotified = true
				return next, &Notification{Event: EventPersistentWarning, Severity: provider.SeverityWarning, Streak: next.Streak}
			}
			return next, nil
		}
		return State{}, nil // OK breaks the series, silently

	case PhaseIncident:
		if verdict == health.StatusOK {
			return State{Phase: PhaseResolving, Streak: 1}, nil
		}
		return State{Phase: PhaseIncident}, nil // degraded or investigate: the incident lasts

	case PhaseResolving:
		switch verdict {
		case health.StatusDegraded:
			// The same incident oscillating: no new notification.
			return State{Phase: PhaseIncident}, nil
		case health.StatusInvestigate:
			// Better, not back to OK: the OK count starts over.
			return State{Phase: PhaseResolving}, nil
		}
		if s.Streak+1 >= okRoundsToResolve {
			return State{}, &Notification{Event: EventResolved, Severity: provider.SeverityInfo}
		}
		return State{Phase: PhaseResolving, Streak: s.Streak + 1}, nil
	}
	return s, nil
}

func incident() *Notification {
	return &Notification{Event: EventIncident, Severity: provider.SeverityCritical}
}

// enterWarning starts a warning series from OK. With a persistence of
// one the series is persistent at once, and that notification wins over
// the plain warning one.
func enterWarning(cfg Config) (State, *Notification) {
	next := State{Phase: PhaseWarning, Streak: 1}
	if cfg.persistence() <= 1 {
		next.WarningNotified = true
		return next, &Notification{Event: EventPersistentWarning, Severity: provider.SeverityWarning, Streak: 1}
	}
	if cfg.NotifyWarning {
		return next, &Notification{Event: EventWarning, Severity: provider.SeverityWarning}
	}
	return next, nil
}
