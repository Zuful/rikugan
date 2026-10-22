package watch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

type fakeInvestigator struct {
	mu   sync.Mutex
	got  []Incident
	gate chan struct{} // when set, each investigation waits for a token
	out  func(Incident) Investigation
}

func (f *fakeInvestigator) Investigate(ctx context.Context, inc Incident) Investigation {
	f.mu.Lock()
	f.got = append(f.got, inc)
	f.mu.Unlock()
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
		}
	}
	if f.out != nil {
		return f.out(inc)
	}
	return Investigation{Outcome: EscalationDone, Reply: provider.TeamMessage{Title: "Investigation", Body: "Probable cause: " + inc.Title},
		Session: "ses_1", Tokens: 1000, Cost: 0.5, Calls: 3}
}

func (f *fakeInvestigator) incidents() []Incident {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Incident(nil), f.got...)
}

// escalating gives the harness's daemon an escalation and a channel
// that returns references, as after a start.
func (h *harness) escalating(e *Escalation, ch *fakeChannel) {
	h.t.Helper()
	h.d.Escalation = e
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{ch}}
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{0: health.StatusDegraded}}
	if err := h.d.Replay(); err != nil {
		h.t.Fatal(err)
	}
}

// finish hands n ended investigations to the daemon, as its loop does.
func (h *harness) finish(n int) {
	h.t.Helper()
	for range n {
		select {
		case res := <-h.d.Escalation.done:
			if err := h.d.finishEscalation(context.Background(), res); err != nil {
				h.t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			h.t.Fatal("no investigation ended")
		}
	}
}

func failedRollout(svc provider.Service, seq string, at time.Duration) provider.Rollout {
	return provider.Rollout{Service: svc, Sequence: seq, Revision: "h" + seq, State: provider.RolloutFailed, StartedAt: t0, FailedAt: t0.Add(at)}
}

func (h *harness) critical(svc provider.Service, seq string, at time.Duration) {
	h.t.Helper()
	r := failedRollout(svc, seq, at)
	h.source.current = r
	h.at(at).handle(r)
}

var web = provider.Service{Name: "web", Namespace: "prod", Kind: "deployment"}

func TestCriticalIsInvestigatedOnceAndAnsweredInItsThread(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{}
	ch := &fakeChannel{name: "telegram", refs: true}
	h.escalating(&Escalation{Investigator: inv, PerHour: 4, Concurrency: 1}, ch)

	h.critical(api, "2", 10*time.Minute)
	h.finish(1)
	got := inv.incidents()
	if len(got) != 1 {
		t.Fatalf("%d investigations, want 1", len(got))
	}
	inc := got[0]
	if inc.Key != "rollout/prod/api/2" || inc.Refs["telegram"] != "1" || len(inc.Services) != 1 || inc.Revisions["prod/api"][0] != "2" {
		t.Errorf("incident %+v", inc)
	}
	reply := ch.msgs[len(ch.msgs)-1]
	if reply.ReplyTo == nil || reply.ReplyTo.Ref != "1" || !strings.Contains(reply.Body, "Probable cause") {
		t.Errorf("reply %+v", reply)
	}
	for _, ev := range []string{EscalationQueued, EscalationStarted, EscalationDone} {
		if h.count(`"event":"`+ev+`"`) != 1 {
			t.Errorf("no %s record:\n%s", ev, strings.Join(h.records(), "\n"))
		}
	}
	if h.count(`"summary":"Probable cause`) != 1 || h.count(`"session":"ses_1"`) != 1 {
		t.Errorf("done record lacks the summary or session:\n%s", strings.Join(h.records(), "\n"))
	}

	// The same incident again, after a restart: no second investigation.
	h.restart()
	h.escalating(&Escalation{Investigator: inv, PerHour: 4, Concurrency: 1}, ch)
	if err := h.d.escalate(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if n := len(inv.incidents()); n != 1 {
		t.Errorf("%d investigations after a repeat, want 1", n)
	}
}

func TestHourlyCapAnswersTheThread(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{}
	ch := &fakeChannel{name: "telegram", refs: true}
	h.escalating(&Escalation{Investigator: inv, PerHour: 1, Concurrency: 1}, ch)

	h.critical(api, "2", 10*time.Minute)
	h.finish(1)
	h.critical(web, "5", 20*time.Minute)
	if n := len(inv.incidents()); n != 1 {
		t.Fatalf("%d investigations, want 1", n)
	}
	last := ch.msgs[len(ch.msgs)-1]
	if last.ReplyTo == nil || !strings.Contains(last.Body, "cap of 1 investigations an hour") {
		t.Errorf("last message %+v", last)
	}
	// An hour later, room again.
	h.critical(web, "6", 75*time.Minute)
	h.finish(1)
	if n := len(inv.incidents()); n != 2 {
		t.Errorf("%d investigations an hour later, want 2", n)
	}
}

// The month's spending comes back from the journal: past the budget, no
// investigation, until the next month.
func TestMonthlyBudgetSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{out: func(Incident) Investigation {
		return Investigation{Outcome: EscalationDone, Reply: provider.TeamMessage{Body: "x"}, Tokens: 400000, Cost: -1}
	}}
	ch := &fakeChannel{name: "telegram", refs: true}
	e := func() *Escalation {
		return &Escalation{Investigator: inv, PerHour: 10, Concurrency: 1, MonthlyBudget: 5, UnknownPricePerMTokens: 15}
	}
	h.escalating(e(), ch)
	h.critical(api, "2", 10*time.Minute) // unknown cost: 400k tokens at $15/M = $6
	h.finish(1)

	h.restart()
	h.escalating(e(), ch)
	h.critical(web, "5", 20*time.Minute)
	if n := len(inv.incidents()); n != 1 {
		t.Fatalf("%d investigations past the budget, want 1", n)
	}
	if last := ch.msgs[len(ch.msgs)-1]; !strings.Contains(last.Body, "budget ($5.00) is spent") {
		t.Errorf("last message %q", last.Body)
	}
}

// One at a time: the second waits, and is dropped when its critical is
// past StaleAfter by the time its turn comes.
func TestQueuedInvestigationGoesStale(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{gate: make(chan struct{})}
	ch := &fakeChannel{name: "telegram", refs: true}
	h.escalating(&Escalation{Investigator: inv, PerHour: 10, Concurrency: 1, StaleAfter: 30 * time.Minute}, ch)

	h.critical(api, "2", 10*time.Minute)
	h.critical(web, "5", 11*time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := len(inv.incidents()); n != 1 {
		t.Fatalf("%d running, want 1 (concurrency 1)", n)
	}
	h.at(50 * time.Minute)
	inv.gate <- struct{}{}
	h.finish(1)
	if n := len(inv.incidents()); n != 1 {
		t.Errorf("%d investigations, want the second dropped as stale", n)
	}
	if h.count(`"event":"stale"`) != 1 {
		t.Errorf("no stale record:\n%s", strings.Join(h.records(), "\n"))
	}
}

// A stop leaves investigations open; the next start says they were
// interrupted, in their thread, and never runs them again.
func TestInterruptedByARestart(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{gate: make(chan struct{})}
	ch := &fakeChannel{name: "telegram", refs: true}
	h.escalating(&Escalation{Investigator: inv, PerHour: 10, Concurrency: 1}, ch)
	h.critical(api, "2", 10*time.Minute)

	h.restart()
	h.escalating(&Escalation{Investigator: inv, PerHour: 10, Concurrency: 1}, ch)
	if err := h.d.resumeEscalations(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := ch.msgs[len(ch.msgs)-1]
	if last.ReplyTo == nil || last.ReplyTo.Ref != "1" || !strings.Contains(last.Body, "interrupted by a restart") {
		t.Errorf("last message %+v", last)
	}
	if h.count(`"event":"interrupted"`) != 1 {
		t.Errorf("no interrupted record:\n%s", strings.Join(h.records(), "\n"))
	}
	// Ended: a second start has nothing to resume.
	h.restart()
	h.escalating(&Escalation{Investigator: inv, PerHour: 10, Concurrency: 1}, ch)
	if err := h.d.resumeEscalations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.count(`"event":"interrupted"`) != 1 {
		t.Error("interrupted twice")
	}
}

// A blocked escalation warns the team once per reason, and each
// critical's thread says it was not investigated.
func TestBlockedWarnsOnce(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{out: func(Incident) Investigation {
		return Investigation{Outcome: EscalationBlocked, Reason: "unknown native tool: browser",
			Reply: provider.TeamMessage{Body: "Not investigated: unknown native tool: browser"}}
	}}
	ch := &fakeChannel{name: "telegram", refs: true}
	h.escalating(&Escalation{Investigator: inv, PerHour: 10, Concurrency: 1}, ch)
	h.critical(api, "2", 10*time.Minute)
	h.finish(1)
	h.restart()
	h.escalating(&Escalation{Investigator: inv, PerHour: 10, Concurrency: 1}, ch)
	h.critical(web, "5", 20*time.Minute)
	h.finish(1)

	var warnings, lines int
	for _, m := range ch.msgs {
		if m.Title == "Escalation to an agent is blocked" {
			warnings++
		}
		if m.ReplyTo != nil && strings.Contains(m.Body, "Not investigated") {
			lines++
		}
	}
	if warnings != 1 || lines != 2 {
		t.Errorf("%d warnings and %d thread lines, want 1 and 2", warnings, lines)
	}
}

// A sweep's grouped critical is one investigation listing every
// service.
func TestSweepIncident(t *testing.T) {
	h := newHarness(t)
	group := []*sweepResult{
		{svc: api, rep: health.Report{Signals: []health.SignalResult{{Name: "error_rate"}}}},
		{svc: web, rep: health.Report{Signals: []health.SignalResult{{Name: "latency_p95"}, {Name: "error_rate"}}}},
	}
	inc := h.d.sweepIncident(group, provider.TeamMessage{Title: "2 services degraded (periodic sweep)", Body: "b"}, nil)
	if !inc.Sweep || !inc.Grouped || len(inc.Services) != 2 || strings.Join(inc.Signals, ",") != "error_rate,latency_p95" ||
		!strings.HasPrefix(inc.Key, "sweep/prod/api,prod/web/") {
		t.Errorf("incident %+v", inc)
	}
}

// A running investigation counts once against the hourly cap: with a
// cap of 2, a second critical waits its turn instead of being capped.
func TestRunningInvestigationCountsOnce(t *testing.T) {
	h := newHarness(t)
	inv := &fakeInvestigator{gate: make(chan struct{}, 2)}
	ch := &fakeChannel{name: "telegram", refs: true}
	h.escalating(&Escalation{Investigator: inv, PerHour: 2, Concurrency: 1}, ch)
	h.critical(api, "2", 10*time.Minute)
	h.critical(web, "5", 11*time.Minute)
	if h.count(`"event":"capped"`) != 0 {
		t.Fatalf("second critical capped:\n%s", strings.Join(h.records(), "\n"))
	}
	inv.gate <- struct{}{}
	inv.gate <- struct{}{}
	h.finish(2)
	if n := len(inv.incidents()); n != 2 {
		t.Errorf("%d investigations, want 2", n)
	}
}
