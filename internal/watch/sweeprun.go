package watch

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/watch/sweep"
	"github.com/Zuful/rikugan/pkg/provider"
)

// KindSweep records one service's sweep result in the journal.
const KindSweep = "sweep"

// DefaultSweepInterval is sweep_interval's default (design §6).
const DefaultSweepInterval = 4 * time.Hour

// SweepChecker evaluates one service for the sweep. *health.Collector
// implements it.
type SweepChecker interface {
	CheckSweep(ctx context.Context, svc provider.Service, prev *health.PreviousSweep) (health.Report, map[string]int, error)
}

// SweepConfig turns on the periodic sweep (design §6): it covers what
// rollouts do not, a slow degradation, a dependency going down, a
// memory leak.
type SweepConfig struct {
	// Interval is sweep_interval; zero means DefaultSweepInterval.
	Interval time.Duration
	// Namespaces are swept; their services are listed through Services.
	Namespaces []string
	// Exclude names services to leave out, as name or namespace/name.
	Exclude  []string
	Services provider.OrchestrationProvider
	Checker  SweepChecker
	// State holds sweep_notify_warning and sweep_warning_persistence.
	State sweep.Config
}

func (c *SweepConfig) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultSweepInterval
}

func (c *SweepConfig) excluded(svc provider.Service) bool {
	for _, e := range c.Exclude {
		if e == svc.Name || e == serviceKey(svc) {
			return true
		}
	}
	return false
}

// sweepServiceState is what the daemon keeps per swept service.
type sweepServiceState struct {
	state sweep.State
	prev  *health.PreviousSweep
}

type sweepRecord struct {
	Revision string            `json:"revision,omitempty"`
	Outcome  string            `json:"outcome"`
	Verdict  string            `json:"verdict,omitempty"`
	Signals  []Signal          `json:"signals,omitempty"`
	Notes    []string          `json:"notes,omitempty"`
	State    sweep.State       `json:"state"`
	Restarts map[string]int    `json:"restarts"`
	Notified *sweepNotifRecord `json:"notified,omitempty"`
}

type sweepNotifRecord struct {
	Event     sweep.Event `json:"event"`
	Severity  string      `json:"severity"`
	Title     string      `json:"title"`
	Delivered []string    `json:"delivered,omitempty"`
}

// replaySweep restores a service's sweep state from its latest record.
func (d *Daemon) replaySweep(service string, at time.Time, data json.RawMessage) {
	var rec sweepRecord
	if json.Unmarshal(data, &rec) != nil {
		return
	}
	if d.sweeps == nil {
		d.sweeps = map[string]*sweepServiceState{}
	}
	d.sweeps[service] = &sweepServiceState{state: rec.State, prev: &health.PreviousSweep{At: at, Restarts: rec.Restarts}}
	if at.After(d.lastSweep) {
		d.lastSweep = at
	}
}

// nextSweep is when the next sweep is due: now on a first start.
func (d *Daemon) nextSweep() time.Time {
	if d.Sweep == nil {
		return time.Time{}
	}
	if d.lastSweep.IsZero() {
		return d.now()
	}
	return d.lastSweep.Add(d.Sweep.interval())
}

// runSweep sweeps every service of the configured namespaces, except the
// excluded ones and those a rollout check still covers.
func (d *Daemon) runSweep(ctx context.Context) error {
	now := d.now()
	d.lastSweep = now // even if some services fail, the next sweep waits an interval
	if d.sweeps == nil {
		d.sweeps = map[string]*sweepServiceState{}
	}
	for _, ns := range d.Sweep.Namespaces {
		services, err := d.Sweep.Services.ListServices(ctx, ns)
		if err != nil {
			d.logger().Warn("sweep: cannot list services", slog.String("namespace", ns), slog.String("error", err.Error()))
			continue
		}
		for _, svc := range services {
			if svc.Namespace == "" {
				svc.Namespace = ns
			}
			if d.Sweep.excluded(svc) {
				continue
			}
			if covered, why := d.coveredByRollout(ctx, svc, now); covered {
				d.logger().Debug("sweep: skipped", slog.String("service", serviceKey(svc)), slog.String("reason", why))
				continue
			}
			if err := d.sweepService(ctx, svc); err != nil {
				return err
			}
		}
	}
	return nil
}

// coveredByRollout reports whether the service's rollout checks still
// cover it: a rollout in progress, or one that ended less than
// stale_after ago (2 h by default, design §6).
func (d *Daemon) coveredByRollout(ctx context.Context, svc provider.Service, now time.Time) (bool, string) {
	r, err := d.Source.LastRollout(ctx, svc)
	if err != nil {
		return false, ""
	}
	limit := d.config(svc).StaleAfter
	switch {
	case r.State == provider.RolloutInProgress:
		return true, "rollout in progress"
	case r.State == provider.RolloutComplete && now.Sub(r.CompletedAt) < limit:
		return true, "rollout completed under " + limit.String() + " ago"
	case r.State == provider.RolloutFailed && now.Sub(r.FailedAt) < limit:
		return true, "rollout failed under " + limit.String() + " ago"
	}
	return false, ""
}

// sweepService checks one service, steps its state machine, notifies
// what the machine says, and records the result. A backend error skips
// the service until the next sweep.
func (d *Daemon) sweepService(ctx context.Context, svc provider.Service) error {
	key := serviceKey(svc)
	st := d.sweeps[key]
	if st == nil {
		st = &sweepServiceState{}
		d.sweeps[key] = st
	}
	rep, counts, err := d.Sweep.Checker.CheckSweep(ctx, svc, st.prev)
	if err != nil {
		d.logger().Warn("sweep: check failed", slog.String("service", key), slog.String("error", err.Error()))
		return nil
	}
	next, note := sweep.Step(st.state, rep.Verdict, d.Sweep.State)

	rec := sweepRecord{Revision: rep.Rollout.Sequence, Outcome: string(rep.Outcome), Verdict: string(rep.Verdict),
		Notes: rep.Notes, State: next, Restarts: counts}
	for _, s := range rep.Signals {
		rec.Signals = append(rec.Signals, Signal{Name: s.Name, Status: string(s.Status), Reason: s.Reason})
	}
	// As for rollouts: send first, so a crash repeats rather than loses.
	if note != nil && d.Notifier != nil {
		msg := d.Notifier.sweepMessage(ctx, svc, rep, *note)
		rec.Notified = &sweepNotifRecord{Event: note.Event, Severity: string(note.Severity), Title: msg.Title,
			Delivered: d.Notifier.Send(ctx, msg)}
	}
	if err := d.append(KindSweep, svc, rec); err != nil {
		return err
	}
	st.state = next
	st.prev = &health.PreviousSweep{At: d.now(), Restarts: counts}
	if d.Verdicts != nil {
		d.Verdicts.Info("sweep", slog.String("service", key), slog.String("verdict", string(rep.Verdict)),
			slog.String("phase", string(next.Phase)))
	}
	return nil
}
