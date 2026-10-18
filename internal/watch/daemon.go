package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/watch/journal"
	"github.com/Zuful/rikugan/pkg/provider"
)

// Journal record kinds written by the daemon.
const (
	KindRollout = "rollout"
	KindCheck   = "check"
	KindSkip    = "checkpoint_skipped"
)

// FailureCheckpoint is the checkpoint under which a failed rollout's
// immediate check is recorded: it does not wait for any checkpoint.
const FailureCheckpoint time.Duration = 0

// lateAfter is how far past its time a check counts as late, which only
// happens when the daemon was down or busy.
const lateAfter = time.Minute

// retryAfter is when a check that could not run (a backend error) is
// tried again.
const retryAfter = time.Minute

// keepRollouts bounds the rollouts remembered per service: enough to
// chain back to the last stable period and to deduplicate events.
const keepRollouts = 20

// Checker evaluates one rollout. *health.Collector implements it.
type Checker interface {
	CheckRollout(ctx context.Context, rollout provider.Rollout, opts health.CheckOptions) (health.Report, error)
}

// Check is one recorded check: a checkpoint of a completed rollout, or
// the immediate check of a failed one (FailureCheckpoint).
type Check struct {
	Rollout    provider.Rollout
	Checkpoint time.Duration
	// Late is set when the check ran past its time, after a restart.
	Late   bool
	Report health.Report
}

// Daemon is the core of rikugan serve: it follows the rollouts a
// DeploymentSource reports, checks each completed rollout at its
// checkpoints and each failed one at once, and records everything in
// the journal, from which it resumes after a restart without running a
// check twice.
type Daemon struct {
	Source  provider.DeploymentSource
	Checker Checker
	Journal *journal.Journal
	// Settings gives a service's thresholds; nil means the defaults.
	Settings func(provider.Service) health.Config
	// Checkpoints defaults to health.Checkpoints.
	Checkpoints []time.Duration
	// Sweep, when set, turns on the periodic sweep.
	Sweep *SweepConfig
	// Notifier, when set, applies the noise policy to each check and
	// sends what it decides.
	Notifier *Notifier
	// OnCheck, when set, is called after each check is recorded.
	OnCheck func(context.Context, Check)
	// Verdicts, when set, receives one structured line per check: the
	// copy that log pipelines (Loki) read.
	Verdicts *slog.Logger
	Logger   *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	services  map[string]*serviceState
	sweeps    map[string]*sweepServiceState
	lastSweep time.Time
}

type serviceState struct {
	rollouts map[string]*rolloutState // by sequence
}

type rolloutState struct {
	rollout  provider.Rollout
	reported map[provider.RolloutState]bool
	checked  map[time.Duration]bool
	skipped  map[time.Duration]bool
	retryAt  time.Time
	notify   notifyState
}

// Payloads of the journal records. They are the journal's own format,
// with stable snake_case keys, rather than the provider and health types
// marshalled as they are: the verdicts API reads them back (PR 5), and a
// rename in Go must not change what is on disk.
type (
	rolloutRecord struct {
		Namespace     string    `json:"namespace"`
		Name          string    `json:"name"`
		Kind          string    `json:"kind,omitempty"`
		Revision      string    `json:"revision,omitempty"`
		Sequence      string    `json:"sequence"`
		Images        []string  `json:"images,omitempty"`
		Rollback      bool      `json:"rollback,omitempty"`
		State         string    `json:"state"`
		StartedAt     time.Time `json:"started_at,omitzero"`
		CompletedAt   time.Time `json:"completed_at,omitzero"`
		FailedAt      time.Time `json:"failed_at,omitzero"`
		FailureReason string    `json:"failure_reason,omitempty"`
	}
	checkRecord struct {
		Sequence   string   `json:"sequence"`
		Checkpoint string   `json:"checkpoint"`
		Late       bool     `json:"late,omitempty"`
		Outcome    string   `json:"outcome"`
		Verdict    string   `json:"verdict,omitempty"`
		Signals    []Signal `json:"signals,omitempty"`
		Notes      []string `json:"notes,omitempty"`
	}
	// Signal is one signal of a recorded check.
	Signal struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
	}
	skipRecord struct {
		Sequence   string `json:"sequence"`
		Checkpoint string `json:"checkpoint"`
		Reason     string `json:"reason"`
	}
	notificationRecord struct {
		Sequence   string `json:"sequence"`
		Checkpoint string `json:"checkpoint"`
		Event      string `json:"event"`
		Severity   string `json:"severity"`
		Title      string `json:"title"`
		// Delivered lists the channels that took the message.
		Delivered []string    `json:"delivered,omitempty"`
		State     notifyState `json:"state"`
	}
)

func toRolloutRecord(r provider.Rollout) rolloutRecord {
	return rolloutRecord{
		Namespace: r.Service.Namespace, Name: r.Service.Name, Kind: r.Service.Kind,
		Revision: r.Revision, Sequence: r.Sequence, Images: r.Images, Rollback: r.Rollback,
		State: string(r.State), StartedAt: r.StartedAt, CompletedAt: r.CompletedAt,
		FailedAt: r.FailedAt, FailureReason: r.FailureReason,
	}
}

func (rec rolloutRecord) rollout() provider.Rollout {
	return provider.Rollout{
		Service:  provider.Service{Namespace: rec.Namespace, Name: rec.Name, Kind: rec.Kind},
		Revision: rec.Revision, Sequence: rec.Sequence, Images: rec.Images, Rollback: rec.Rollback,
		State: provider.RolloutState(rec.State), StartedAt: rec.StartedAt, CompletedAt: rec.CompletedAt,
		FailedAt: rec.FailedAt, FailureReason: rec.FailureReason,
	}
}

func toCheckRecord(r provider.Rollout, cp time.Duration, late bool, rep health.Report) checkRecord {
	rec := checkRecord{Sequence: r.Sequence, Checkpoint: cp.String(), Late: late,
		Outcome: string(rep.Outcome), Verdict: string(rep.Verdict), Notes: rep.Notes}
	for _, s := range rep.Signals {
		rec.Signals = append(rec.Signals, Signal{Name: s.Name, Status: string(s.Status), Reason: s.Reason})
	}
	return rec
}

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Daemon) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d *Daemon) checkpoints() []time.Duration {
	if len(d.Checkpoints) > 0 {
		return d.Checkpoints
	}
	return health.Checkpoints
}

func (d *Daemon) config(svc provider.Service) health.Config {
	if d.Settings != nil {
		return d.Settings(svc)
	}
	return health.DefaultConfig()
}

func serviceKey(svc provider.Service) string { return svc.Namespace + "/" + svc.Name }

// Run replays the journal, then follows the source's rollouts and the
// checkpoints until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Replay(); err != nil {
		return err
	}
	events, err := d.Source.WatchRollouts(ctx)
	if err != nil {
		return fmt.Errorf("watch rollouts: %w", err)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("rollout watch ended")
			}
			if err := d.Handle(ctx, r); err != nil {
				return err
			}
		case <-timer.C:
		}
		next, err := d.RunDue(ctx)
		if err != nil {
			return err
		}
		wait := time.Hour
		if !next.IsZero() {
			wait = max(next.Sub(d.now()), 0)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
	}
}

// Replay rebuilds the daemon's state from the journal.
func (d *Daemon) Replay() error {
	d.services = map[string]*serviceState{}
	d.sweeps = map[string]*sweepServiceState{}
	d.lastSweep = time.Time{}
	skipped, err := d.Journal.Replay(func(rec journal.Record) error {
		switch rec.Kind {
		case KindRollout:
			var rr rolloutRecord
			if json.Unmarshal(rec.Data, &rr) == nil {
				r := rr.rollout()
				d.rolloutState(r).apply(r)
			}
		case KindSweep:
			d.replaySweep(rec.Service, rec.Time, rec.Data)
		case KindNotification:
			var n notificationRecord
			if json.Unmarshal(rec.Data, &n) != nil {
				return nil
			}
			if st := d.services[rec.Service]; st != nil && st.rollouts[n.Sequence] != nil {
				st.rollouts[n.Sequence].notify = n.State
			}
		case KindCheck, KindSkip:
			var c skipRecord // shares the sequence and checkpoint fields
			if json.Unmarshal(rec.Data, &c) != nil {
				return nil
			}
			cp, err := time.ParseDuration(c.Checkpoint)
			if err != nil {
				return nil
			}
			st := d.services[rec.Service]
			if st == nil || st.rollouts[c.Sequence] == nil {
				return nil
			}
			rs := st.rollouts[c.Sequence]
			if rec.Kind == KindCheck {
				rs.checked[cp] = true
			} else {
				rs.skipped[cp] = true
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replay journal: %w", err)
	}
	if skipped > 0 {
		d.logger().Warn("journal lines skipped on replay", slog.Int("lines", skipped))
	}
	return nil
}

func (d *Daemon) rolloutState(r provider.Rollout) *rolloutState {
	if d.services == nil {
		d.services = map[string]*serviceState{}
	}
	key := serviceKey(r.Service)
	st := d.services[key]
	if st == nil {
		st = &serviceState{rollouts: map[string]*rolloutState{}}
		d.services[key] = st
	}
	rs := st.rollouts[r.Sequence]
	if rs == nil {
		rs = &rolloutState{
			rollout:  r,
			reported: map[provider.RolloutState]bool{},
			checked:  map[time.Duration]bool{},
			skipped:  map[time.Duration]bool{},
		}
		st.rollouts[r.Sequence] = rs
		st.prune()
	}
	return rs
}

// Handle records a rollout reported by the source, once per (sequence,
// state). A newer rollout cancels the pending checkpoints of the older
// ones; a failed rollout is checked at once.
func (d *Daemon) Handle(ctx context.Context, r provider.Rollout) error {
	rs := d.rolloutState(r)
	if rs.reported[r.State] {
		return nil // already recorded, typically re-reported at start
	}
	if err := d.append(KindRollout, r.Service, toRolloutRecord(r)); err != nil {
		return err
	}
	rs.apply(r)

	st := d.services[serviceKey(r.Service)]
	for _, older := range st.rollouts {
		if older != rs && newer(rs.rollout, older.rollout) {
			if err := d.skipPending(older, "superseded by revision "+r.Sequence); err != nil {
				return err
			}
		}
	}

	if r.State == provider.RolloutFailed && !rs.checked[FailureCheckpoint] {
		// A failure older than stale_after, first seen when the daemon
		// starts, is recorded but not checked: it is not news.
		if d.now().Sub(r.FailedAt) > d.config(r.Service).StaleAfter {
			return nil
		}
		return d.check(ctx, rs, FailureCheckpoint, false)
	}
	return nil
}

// apply takes a reported state of the rollout.
func (rs *rolloutState) apply(r provider.Rollout) {
	rs.rollout = r
	rs.reported[r.State] = true
}

// RunDue runs the checks whose time has come, and the periodic sweep
// when it is due, and returns when the next one is due (zero when none
// is pending).
//
// After a restart, several checkpoints of a rollout may be overdue:
// only the latest is run, late, and the others are recorded as skipped,
// so a restart never sends a burst of checks for one rollout. A rollout
// older than its stale_after is not checked any more.
func (d *Daemon) RunDue(ctx context.Context) (time.Time, error) {
	now := d.now()
	var next time.Time
	for _, key := range d.serviceKeys() {
		st := d.services[key]
		for _, rs := range st.sorted() {
			r := rs.rollout
			if r.State != provider.RolloutComplete || r.CompletedAt.IsZero() {
				continue
			}
			pending := rs.pending(d.checkpoints())
			if len(pending) == 0 {
				continue
			}
			if now.Sub(r.CompletedAt) > d.config(r.Service).StaleAfter {
				if err := d.skipPending(rs, "stale: the daemon was down past stale_after"); err != nil {
					return time.Time{}, err
				}
				continue
			}
			var due []time.Duration
			for _, cp := range pending {
				if !r.CompletedAt.Add(cp).After(now) {
					due = append(due, cp)
				} else if at := r.CompletedAt.Add(cp); next.IsZero() || at.Before(next) {
					next = at
				}
			}
			if len(due) == 0 {
				continue
			}
			if now.Before(rs.retryAt) {
				if next.IsZero() || rs.retryAt.Before(next) {
					next = rs.retryAt
				}
				continue
			}
			if superseded, by := d.supersededNow(ctx, rs); superseded {
				if err := d.skipPending(rs, "superseded by revision "+by+", in progress"); err != nil {
					return time.Time{}, err
				}
				continue
			}
			// An overdue checkpoint is only skipped when it is really
			// late: two checkpoints due within lateAfter of each other
			// (a slow check, a busy loop) both run.
			for _, cp := range due[:len(due)-1] {
				if now.Sub(r.CompletedAt.Add(cp)) > lateAfter {
					if err := d.skip(rs, cp, "missed while the daemon was down"); err != nil {
						return time.Time{}, err
					}
				} else if err := d.check(ctx, rs, cp, false); err != nil {
					return time.Time{}, err
				}
			}
			cp := due[len(due)-1]
			late := now.Sub(r.CompletedAt.Add(cp)) > lateAfter
			if err := d.check(ctx, rs, cp, late); err != nil {
				return time.Time{}, err
			}
			if !rs.checked[cp] && (next.IsZero() || rs.retryAt.Before(next)) {
				next = rs.retryAt
			}
		}
	}
	if d.Sweep != nil {
		if !d.nextSweep().After(now) {
			if err := d.runSweep(ctx); err != nil {
				return time.Time{}, err
			}
		}
		if due := d.nextSweep(); next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next, nil
}

// supersededNow asks the source whether a newer rollout has started.
// The source only reports rollouts once they end, so without this a
// rollout replaced ten minutes after completing would still be checked
// at +15 and +30, against metrics the newer rollout now drives.
func (d *Daemon) supersededNow(ctx context.Context, rs *rolloutState) (bool, string) {
	cur, err := d.Source.LastRollout(ctx, rs.rollout.Service)
	if err != nil {
		d.logger().Warn("cannot tell whether a newer rollout started; checking anyway",
			slog.String("service", serviceKey(rs.rollout.Service)), slog.String("error", err.Error()))
		return false, ""
	}
	if cur.Sequence != rs.rollout.Sequence && newer(cur, rs.rollout) {
		return true, cur.Sequence
	}
	return false, ""
}

// check runs one check and records it. A backend error is not fatal:
// the check is retried a minute later.
func (d *Daemon) check(ctx context.Context, rs *rolloutState, cp time.Duration, late bool) error {
	r := rs.rollout
	opts := health.CheckOptions{Checkpoint: cp, BeforeEnd: d.beforeEnd(rs)}
	report, err := d.Checker.CheckRollout(ctx, r, opts)
	if err != nil {
		rs.retryAt = d.now().Add(retryAfter)
		d.logger().Warn("check failed, retrying", slog.String("service", serviceKey(r.Service)),
			slog.String("revision", r.Sequence), slog.Duration("checkpoint", cp), slog.String("error", err.Error()))
		return nil
	}
	c := Check{Rollout: r, Checkpoint: cp, Late: late, Report: report}

	// Send before recording the check: a crash in between then repeats
	// the check and may send twice, rather than recording a check whose
	// critical was never sent.
	var note *notificationRecord
	if d.Notifier != nil && report.Outcome == health.OutcomeEvaluated {
		next, dec := decide(rs.notify, report.Verdict, d.Notifier.NotifyOnOK)
		if dec != nil {
			msg := d.Notifier.message(ctx, c, *dec)
			note = &notificationRecord{
				Sequence: r.Sequence, Checkpoint: cp.String(), Event: dec.Event, Severity: string(dec.Severity),
				Title: msg.Title, Delivered: d.Notifier.Send(ctx, msg), State: next,
			}
		}
	}
	if err := d.append(KindCheck, r.Service, toCheckRecord(r, cp, late, report)); err != nil {
		return err
	}
	rs.checked[cp] = true
	if note != nil {
		if err := d.append(KindNotification, r.Service, note); err != nil {
			return err
		}
		rs.notify = note.State
	}
	if d.Verdicts != nil {
		d.Verdicts.Info("check",
			slog.String("service", serviceKey(r.Service)), slog.String("revision", r.Sequence),
			slog.String("checkpoint", cp.String()), slog.Bool("late", late),
			slog.String("outcome", string(report.Outcome)), slog.String("verdict", string(report.Verdict)))
	}
	if d.OnCheck != nil {
		d.OnCheck(ctx, c)
	}
	return nil
}

// beforeEnd returns where the before window must end when this rollout
// follows others closely: at the start of the earliest rollout of the
// chain whose starts are each less than the before window apart, the
// last stable period known (decision C1). Zero when no earlier rollout
// started within the before window.
func (d *Daemon) beforeEnd(rs *rolloutState) time.Time {
	window := d.config(rs.rollout.Service).BeforeWindow
	st := d.services[serviceKey(rs.rollout.Service)]
	end := rs.rollout.StartedAt
	for {
		var prev time.Time
		for _, o := range st.rollouts {
			s := o.rollout.StartedAt
			if s.IsZero() || !s.Before(end) || end.Sub(s) >= window {
				continue
			}
			if prev.IsZero() || s.Before(prev) {
				prev = s
			}
		}
		if prev.IsZero() {
			break
		}
		end = prev
	}
	if end.Equal(rs.rollout.StartedAt) {
		return time.Time{}
	}
	return end
}

func (d *Daemon) skipPending(rs *rolloutState, reason string) error {
	if rs.rollout.State != provider.RolloutComplete {
		return nil
	}
	for _, cp := range rs.pending(d.checkpoints()) {
		if err := d.skip(rs, cp, reason); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) skip(rs *rolloutState, cp time.Duration, reason string) error {
	r := rs.rollout
	if err := d.append(KindSkip, r.Service, skipRecord{Sequence: r.Sequence, Checkpoint: cp.String(), Reason: reason}); err != nil {
		return err
	}
	rs.skipped[cp] = true
	d.logger().Info("checkpoint skipped", slog.String("service", serviceKey(r.Service)),
		slog.String("revision", r.Sequence), slog.Duration("checkpoint", cp), slog.String("reason", reason))
	return nil
}

func (d *Daemon) append(kind string, svc provider.Service, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s record: %w", kind, err)
	}
	if err := d.Journal.Append(journal.Record{Time: d.now(), Kind: kind, Service: serviceKey(svc), Data: data}); err != nil {
		return fmt.Errorf("record %s: %w", kind, err)
	}
	return nil
}

func (d *Daemon) serviceKeys() []string {
	keys := make([]string, 0, len(d.services))
	for k := range d.services {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pending lists the checkpoints neither checked nor skipped, in order.
func (rs *rolloutState) pending(checkpoints []time.Duration) []time.Duration {
	var out []time.Duration
	for _, cp := range checkpoints {
		if !rs.checked[cp] && !rs.skipped[cp] {
			out = append(out, cp)
		}
	}
	return out
}

// sorted returns the service's rollouts, oldest first.
func (st *serviceState) sorted() []*rolloutState {
	out := make([]*rolloutState, 0, len(st.rollouts))
	for _, rs := range st.rollouts {
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return newer(out[j].rollout, out[i].rollout) })
	return out
}

// prune forgets the oldest rollouts past keepRollouts.
func (st *serviceState) prune() {
	if len(st.rollouts) <= keepRollouts {
		return
	}
	all := st.sorted()
	for _, rs := range all[:len(all)-keepRollouts] {
		delete(st.rollouts, rs.rollout.Sequence)
	}
}

// newer reports whether a is a later rollout than b: by revision number
// when both have one, by start time otherwise.
func newer(a, b provider.Rollout) bool {
	na, errA := strconv.Atoi(a.Sequence)
	nb, errB := strconv.Atoi(b.Sequence)
	if errA == nil && errB == nil {
		return na > nb
	}
	return a.StartedAt.After(b.StartedAt)
}
