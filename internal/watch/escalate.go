package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// KindEscalation records an investigation's progress in the journal:
// queued, started, then how it ended.
const KindEscalation = "escalation"

// Escalation events. An investigation is queued, then started or
// dropped (capped, stale), then ends: done, failed, timeout, blocked,
// or interrupted when the daemon stopped before it ended.
const (
	EscalationQueued      = "queued"
	EscalationStarted     = "started"
	EscalationDone        = "done"
	EscalationFailed      = "failed"
	EscalationTimeout     = "timeout"
	EscalationBlocked     = "blocked"
	EscalationCapped      = "capped"
	EscalationStale       = "stale"
	EscalationInterrupted = "interrupted"
)

// Incident is a delivered critical, handed to the investigator.
type Incident struct {
	// Key identifies the incident: one investigation each.
	Key string `json:"key"`
	// Title is the critical's title, Body its text: the signals and
	// their reasons, the notes, the alerts already firing.
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Refs are the critical's references, by channel, to answer in
	// its thread.
	Refs map[string]provider.MessageRef `json:"refs,omitempty"`
	// At is when the critical was sent.
	At       time.Time          `json:"at"`
	Services []provider.Service `json:"services"`
	// Revisions are the revisions known for each service (namespace/
	// name), the one rolled out first.
	Revisions map[string][]string `json:"revisions,omitempty"`
	Signals   []string            `json:"signals,omitempty"`
	// MetricsDown is set when the critical was decided without the
	// metrics backend.
	MetricsDown bool `json:"metrics_down,omitempty"`
	// Sweep is set for a periodic sweep's critical, Grouped when it
	// lists several services: look for a common cause.
	Sweep   bool `json:"sweep,omitempty"`
	Grouped bool `json:"grouped,omitempty"`
}

// Investigation is how an investigation ended.
type Investigation struct {
	// Outcome is EscalationDone, Failed, Timeout or Blocked.
	Outcome string
	// Reply answers the critical: the investigation's message, or a
	// line saying why there is none.
	Reply provider.TeamMessage
	// Reason explains a blocked investigation; the team is warned once
	// per reason.
	Reason  string
	Session string
	Tokens  int
	// Cost in dollars, negative when unknown.
	Cost  float64
	Calls int
}

// Investigator runs the investigation of a critical. It must stop when
// ctx is done.
type Investigator interface {
	Investigate(ctx context.Context, inc Incident) Investigation
}

// Escalation configures the investigations the daemon asks for each
// delivered critical.
type Escalation struct {
	Investigator Investigator
	// PerHour caps the investigations started in any hour.
	PerHour int
	// MonthlyBudget caps the spending of a calendar month (UTC), in
	// dollars. An investigation whose cost is unknown counts its tokens
	// at UnknownPricePerMTokens.
	MonthlyBudget          float64
	UnknownPricePerMTokens float64
	// Concurrency is how many investigations run at once.
	Concurrency int
	// StaleAfter drops an investigation whose critical is older than
	// this when its turn comes.
	StaleAfter time.Duration

	queue   []Incident
	running int
	done    chan escalationResult
	// From the journal:
	seen    map[string]bool
	open    map[string]escalationRecord // queued or started, not ended
	starts  []time.Time
	spent   map[string]float64 // by month, "2006-01"
	blocked map[string]bool    // reasons warned about
}

type escalationResult struct {
	inc Incident
	inv Investigation
}

type escalationRecord struct {
	Event    string   `json:"event"`
	Incident Incident `json:"incident"`
	Reason   string   `json:"reason,omitempty"`
	Session  string   `json:"session,omitempty"`
	Tokens   int      `json:"tokens,omitempty"`
	Cost     *float64 `json:"cost,omitempty"`
	Calls    int      `json:"calls,omitempty"`
	// Summary is the message posted: filtered by the investigator, and
	// a hypothesis, not a fact.
	Summary string `json:"summary,omitempty"`
}

func (e *Escalation) reset() {
	e.queue, e.running = nil, 0
	e.done = make(chan escalationResult, max(e.Concurrency, 1))
	e.seen, e.open, e.starts = map[string]bool{}, map[string]escalationRecord{}, nil
	e.spent, e.blocked = map[string]float64{}, map[string]bool{}
}

func (e *Escalation) cost(r escalationRecord) float64 {
	if r.Cost != nil && *r.Cost >= 0 {
		return *r.Cost
	}
	return float64(r.Tokens) / 1e6 * e.UnknownPricePerMTokens
}

func (d *Daemon) replayEscalation(at time.Time, data []byte) {
	e := d.Escalation
	var r escalationRecord
	if e == nil || json.Unmarshal(data, &r) != nil {
		return
	}
	key := r.Incident.Key
	e.seen[key] = true
	switch r.Event {
	case EscalationQueued:
		e.open[key] = r
	case EscalationStarted:
		e.open[key] = r
		e.starts = append(e.starts, at)
	case EscalationBlocked:
		e.blocked[r.Reason] = true
		delete(e.open, key)
	default:
		delete(e.open, key)
		e.spent[at.UTC().Format("2006-01")] += e.cost(r)
	}
}

// resumeEscalations ends the investigations a stop left open: they are
// never run again, and their thread says so.
func (d *Daemon) resumeEscalations(ctx context.Context) error {
	e := d.Escalation
	keys := make([]string, 0, len(e.open))
	for k := range e.open {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		inc := e.open[k].Incident
		d.escalationReply(ctx, inc, "Investigation interrupted by a restart of rikugan serve; it is not run again.")
		if err := d.recordEscalation(escalationRecord{Event: EscalationInterrupted, Incident: inc}); err != nil {
			return err
		}
		delete(e.open, k)
	}
	return nil
}

// escalate queues the investigation of a delivered critical, unless the
// incident had one or a cap stops it; the thread is told of a cap.
func (d *Daemon) escalate(ctx context.Context, inc Incident) error {
	e := d.Escalation
	if e == nil || e.seen[inc.Key] {
		return nil
	}
	e.seen[inc.Key] = true
	if reason := d.capReached(); reason != "" {
		d.escalationReply(ctx, inc, "No investigation: "+reason+".")
		return d.recordEscalation(escalationRecord{Event: EscalationCapped, Incident: inc, Reason: reason})
	}
	if err := d.recordEscalation(escalationRecord{Event: EscalationQueued, Incident: inc}); err != nil {
		return err
	}
	e.open[inc.Key] = escalationRecord{Event: EscalationQueued, Incident: inc}
	e.queue = append(e.queue, inc)
	return d.dispatchEscalations(ctx)
}

func (d *Daemon) capReached() string {
	e := d.Escalation
	now := d.now()
	recent := e.starts[:0]
	for _, t := range e.starts {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	e.starts = recent
	if e.PerHour > 0 && len(e.starts)+len(e.queue) >= e.PerHour { // started ones are in starts
		return fmt.Sprintf("the cap of %d investigations an hour is reached", e.PerHour)
	}
	if spent := e.spent[now.UTC().Format("2006-01")]; e.MonthlyBudget > 0 && spent >= e.MonthlyBudget {
		return fmt.Sprintf("this month's budget ($%.2f) is spent", e.MonthlyBudget)
	}
	return ""
}

// dispatchEscalations starts queued investigations while there is room,
// dropping those whose critical went stale while they waited.
func (d *Daemon) dispatchEscalations(ctx context.Context) error {
	e := d.Escalation
	for len(e.queue) > 0 && e.running < max(e.Concurrency, 1) {
		inc := e.queue[0]
		e.queue = e.queue[1:]
		if e.StaleAfter > 0 && d.now().Sub(inc.At) > e.StaleAfter {
			d.escalationReply(ctx, inc, fmt.Sprintf("No investigation: its turn came %s after the critical, past %s.",
				d.now().Sub(inc.At).Round(time.Minute), e.StaleAfter))
			delete(e.open, inc.Key)
			if err := d.recordEscalation(escalationRecord{Event: EscalationStale, Incident: inc}); err != nil {
				return err
			}
			continue
		}
		if err := d.recordEscalation(escalationRecord{Event: EscalationStarted, Incident: inc}); err != nil {
			return err
		}
		e.open[inc.Key] = escalationRecord{Event: EscalationStarted, Incident: inc}
		e.starts = append(e.starts, d.now())
		e.running++
		go func() { e.done <- escalationResult{inc, e.Investigator.Investigate(ctx, inc)} }()
	}
	return nil
}

// finishEscalation posts and records an investigation's end, then
// starts the next one.
func (d *Daemon) finishEscalation(ctx context.Context, res escalationResult) error {
	e := d.Escalation
	e.running--
	inv := res.inv
	if ctx.Err() != nil {
		return nil // stopped: left open, so the next start says interrupted
	}
	if inv.Outcome == EscalationBlocked && inv.Reason != "" && !e.blocked[inv.Reason] && d.Notifier != nil {
		e.blocked[inv.Reason] = true
		d.Notifier.Send(ctx, provider.TeamMessage{
			Severity: provider.SeverityWarning,
			Title:    "Escalation to an agent is blocked",
			Body:     inv.Reason + "\nCriticals are still sent; they are not investigated until this is fixed.",
		})
	}
	if d.Notifier != nil && (inv.Reply.Body != "" || inv.Reply.Title != "") {
		d.Notifier.Reply(ctx, res.inc.Refs, res.inc.Title, inv.Reply)
	}
	cost := inv.Cost
	rec := escalationRecord{
		Event: inv.Outcome, Incident: res.inc, Reason: inv.Reason, Session: inv.Session,
		Tokens: inv.Tokens, Cost: &cost, Calls: inv.Calls,
	}
	if inv.Outcome == EscalationDone {
		rec.Summary = inv.Reply.Body
	}
	delete(e.open, res.inc.Key)
	e.spent[d.now().UTC().Format("2006-01")] += e.cost(rec)
	if err := d.recordEscalation(rec); err != nil {
		return err
	}
	d.logger().Info("investigation ended", slog.String("incident", res.inc.Key), slog.String("outcome", inv.Outcome),
		slog.String("session", inv.Session), slog.Int("tokens", inv.Tokens), slog.Int("calls", inv.Calls))
	return d.dispatchEscalations(ctx)
}

// escalationReply answers the critical with a line; empty does nothing.
func (d *Daemon) escalationReply(ctx context.Context, inc Incident, line string) {
	if line == "" || d.Notifier == nil {
		return
	}
	d.Notifier.Reply(ctx, inc.Refs, inc.Title, provider.TeamMessage{Severity: provider.SeverityInfo, Title: "Investigation", Body: line})
}

func (d *Daemon) recordEscalation(r escalationRecord) error {
	svc := provider.Service{}
	if len(r.Incident.Services) == 1 {
		svc = r.Incident.Services[0]
	}
	return d.append(KindEscalation, svc, r)
}

// rolloutIncident builds the incident of a rollout's critical.
func (d *Daemon) rolloutIncident(c Check, note *notificationRecord, body string, partial bool) Incident {
	r := c.Rollout
	key := serviceKey(r.Service)
	inc := Incident{
		Key:         "rollout/" + key + "/" + r.Label(),
		Title:       note.Title,
		Body:        body,
		Refs:        note.Refs,
		At:          d.now(),
		Services:    []provider.Service{r.Service},
		Revisions:   map[string][]string{key: d.knownRevisions(r)},
		MetricsDown: partial,
	}
	for _, s := range c.Report.Signals {
		inc.Signals = append(inc.Signals, s.Name)
	}
	return inc
}

// knownRevisions lists the revisions the daemon knows for r's service,
// r's first, then the others newest first: the targets a rollback may
// name.
func (d *Daemon) knownRevisions(r provider.Rollout) []string {
	out := []string{r.Label()}
	st := d.services[serviceKey(r.Service)]
	if st == nil {
		return out
	}
	var others []*rolloutState
	for _, rs := range st.rollouts {
		if rs.rollout.Label() != r.Label() {
			others = append(others, rs)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].rollout.StartedAt.After(others[j].rollout.StartedAt) })
	for i, rs := range others {
		if i == 4 {
			break
		}
		out = append(out, rs.rollout.Label())
	}
	return out
}

// sweepIncident builds the incident of a sweep's critical, one or
// several services.
func (d *Daemon) sweepIncident(group []*sweepResult, msg provider.TeamMessage, refs map[string]provider.MessageRef) Incident {
	inc := Incident{
		Title: msg.Title, Body: msg.Body, Refs: refs, At: d.now(),
		Revisions: map[string][]string{}, Sweep: true, Grouped: len(group) > 1,
	}
	var keys []string
	signals := map[string]bool{}
	for _, r := range group {
		inc.Services = append(inc.Services, r.svc)
		keys = append(keys, serviceKey(r.svc))
		for _, s := range r.rep.Signals {
			signals[s.Name] = true
		}
	}
	for s := range signals {
		inc.Signals = append(inc.Signals, s)
	}
	sort.Strings(inc.Signals)
	sort.Strings(keys)
	inc.Key = "sweep/" + strings.Join(keys, ",") + "/" + d.now().UTC().Format(time.RFC3339)
	return inc
}
