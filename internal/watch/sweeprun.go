package watch

import (
	"context"
	"encoding/json"
	"errors"
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
	// retryAt is set when no channel took the service's last sweep
	// message: that sweep is not recorded and runs again then.
	retryAt time.Time
	svc     provider.Service
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
	var swept []provider.Service
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
			swept = append(swept, svc)
		}
	}
	return d.sweepServices(ctx, swept)
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

// retrySweeps runs again the sweeps whose message no channel took. It
// returns when the next retry is due, zero when none is.
func (d *Daemon) retrySweeps(ctx context.Context) (time.Time, error) {
	var due []provider.Service
	for _, st := range d.sweeps {
		if !st.retryAt.IsZero() && !st.retryAt.After(d.now()) {
			due = append(due, st.svc)
		}
	}
	if err := d.sweepServices(ctx, due); err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, st := range d.sweeps {
		if !st.retryAt.IsZero() && (next.IsZero() || st.retryAt.Before(next)) {
			next = st.retryAt
		}
	}
	return next, nil
}

// sweepResult is one service's sweep, evaluated, not yet recorded.
type sweepResult struct {
	svc  provider.Service
	st   *sweepServiceState
	rep  health.Report
	rec  sweepRecord
	next sweep.State
	note *sweep.Notification
}

// sweepServices checks each service, steps its state machine, notifies
// what the machines say, and records the results. Services that reach
// the same event in one sweep share one message: a node or a shared
// dependency going down must not send fifteen criticals at once, and
// channels limit how fast they take messages anyway.
func (d *Daemon) sweepServices(ctx context.Context, services []provider.Service) error {
	var results []*sweepResult
	for _, svc := range services {
		if r := d.evaluateSweep(ctx, svc); r != nil {
			results = append(results, r)
		}
	}

	// As for rollouts: send first, so a crash repeats rather than loses.
	undelivered := map[*sweepResult]bool{}
	if d.Notifier != nil {
		var events []sweep.Event
		byEvent := map[sweep.Event][]*sweepResult{}
		for _, r := range results {
			if r.note == nil {
				continue
			}
			if byEvent[r.note.Event] == nil {
				events = append(events, r.note.Event)
			}
			byEvent[r.note.Event] = append(byEvent[r.note.Event], r)
		}
		for _, ev := range events {
			group := byEvent[ev]
			var msg provider.TeamMessage
			if len(group) == 1 {
				msg = d.Notifier.sweepMessage(ctx, group[0].svc, group[0].rep, *group[0].note)
			} else {
				msg = d.Notifier.sweepGroupMessage(ctx, group)
			}
			delivered := d.Notifier.Send(ctx, msg)
			for _, r := range group {
				r.rec.Notified = &sweepNotifRecord{Event: r.note.Event, Severity: string(r.note.Severity), Title: msg.Title, Delivered: delivered}
				if len(delivered) == 0 && len(d.Notifier.Channels) > 0 {
					undelivered[r] = true
					r.st.retryAt = d.now().Add(retryAfter)
				}
			}
			if len(delivered) == 0 && len(d.Notifier.Channels) > 0 {
				d.logger().Error("sweep: notification not delivered on any channel, retrying",
					slog.String("title", msg.Title), slog.Int("services", len(group)))
			}
		}
	}

	for _, r := range results {
		if undelivered[r] {
			continue
		}
		if err := d.append(KindSweep, r.svc, r.rec); err != nil {
			return err
		}
		r.st.state = r.next
		r.st.prev = &health.PreviousSweep{At: d.now(), Restarts: r.rec.Restarts}
		if d.Verdicts != nil {
			d.Verdicts.Info("sweep", slog.String("service", serviceKey(r.svc)), slog.String("verdict", string(r.rep.Verdict)),
				slog.String("phase", string(r.next.Phase)))
		}
	}
	return nil
}

// evaluateSweep checks one service and steps its state machine, nil
// when it cannot be evaluated. An orchestration error skips the service
// until the next sweep; with the metrics backend failing, the instance
// signals are still evaluated.
func (d *Daemon) evaluateSweep(ctx context.Context, svc provider.Service) *sweepResult {
	key := serviceKey(svc)
	st := d.sweeps[key]
	if st == nil {
		st = &sweepServiceState{}
		d.sweeps[key] = st
	}
	st.retryAt, st.svc = time.Time{}, svc
	rep, counts, err := d.Sweep.Checker.CheckSweep(ctx, svc, st.prev)
	if err != nil {
		d.logger().Warn("sweep: check failed", slog.String("service", key), slog.String("error", err.Error()))
		var be *health.BackendError
		if !errors.As(err, &be) {
			return nil
		}
		d.backendFailed(ctx, be, d.config(svc).StaleAfter)
		if be.Kind != health.BackendMetrics {
			return nil
		}
		d.backendOK(ctx, health.BackendOrchestration)
	} else {
		d.backendOK(ctx, health.BackendOrchestration)
		d.backendOK(ctx, health.BackendMetrics)
	}
	next, note := sweep.Step(st.state, rep.Verdict, d.Sweep.State)
	rec := sweepRecord{Revision: rep.Rollout.Label(), Outcome: string(rep.Outcome), Verdict: string(rep.Verdict),
		Notes: rep.Notes, State: next, Restarts: counts}
	for _, s := range rep.Signals {
		rec.Signals = append(rec.Signals, Signal{Name: s.Name, Status: string(s.Status), Reason: s.Reason})
	}
	return &sweepResult{svc: svc, st: st, rep: rep, rec: rec, next: next, note: note}
}
