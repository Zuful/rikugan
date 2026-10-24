package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

const (
	ok  = health.StatusOK
	inv = health.StatusInvestigate
	deg = health.StatusDegraded
	ins = health.StatusInsufficientData
)

func policy(notifyOnOK bool, verdicts ...health.Status) string {
	var st notifyState
	var out []string
	for _, v := range verdicts {
		var d *decision
		st, d = decide(st, v, notifyOnOK)
		if d == nil {
			out = append(out, "-")
		} else {
			out = append(out, d.Event+"/"+string(d.Severity))
		}
	}
	return strings.Join(out, " ")
}

func TestNoisePolicy(t *testing.T) {
	tests := []struct {
		name     string
		verdicts []health.Status
		want     string
	}{
		// The two cases the pre-phase-2 review asked for.
		{"critical, then insufficient data: no recovered message", []health.Status{deg, ins, ins}, "verdict/critical - -"},
		{"critical, investigate (recovered), degraded again: a new critical", []health.Status{deg, inv, deg}, "verdict/critical recovered/info verdict/critical"},

		{"one notification per rollout: degraded stays quiet", []health.Status{deg, deg, deg}, "verdict/critical - -"},
		{"recovered once, then quiet", []health.Status{deg, ok, ok, inv}, "verdict/critical recovered/info - verdict/warning"},
		{"warning, then quiet while it lasts", []health.Status{inv, inv, ok}, "verdict/warning - -"},
		{"warning that gets worse: critical", []health.Status{inv, deg}, "verdict/warning verdict/critical"},
		{"a warning is not a critical: no recovered after it", []health.Status{inv, ok}, "verdict/warning -"},
		{"ok and insufficient data are not notified by default", []health.Status{ok, ins, ok}, "- - -"},
		{"insufficient data then degraded", []health.Status{ins, deg}, "- verdict/critical"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy(false, tt.verdicts...); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
	if got := policy(true, ok, ok, inv); got != "verdict/info - verdict/warning" {
		t.Errorf("notify_on_ok: %s", got)
	}
}

type fakeChannel struct {
	provider.CommunicationProvider
	name  string
	mu    sync.Mutex
	fails int // failures before succeeding; -1 always fails
	msgs  []provider.TeamMessage
	// refs makes the channel return a reference per message: its
	// number, from 1.
	refs bool
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) PostMessage(_ context.Context, m provider.TeamMessage) (provider.MessageRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails != 0 {
		if f.fails > 0 {
			f.fails--
		}
		return "", errors.New("telegram unreachable")
	}
	f.msgs = append(f.msgs, m)
	if !f.refs {
		return "", nil
	}
	return provider.MessageRef(strconv.Itoa(len(f.msgs))), nil
}

func (f *fakeChannel) titles() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs {
		out = append(out, fmt.Sprintf("[%s] %s", m.Severity, m.Title))
	}
	return strings.Join(out, "\n")
}

type fakeAlerts struct {
	provider.AlertsProvider
	alerts []provider.Alert
	err    error
	asked  string
}

func (f *fakeAlerts) ListActiveAlerts(_ context.Context, service string) ([]provider.Alert, error) {
	f.asked = service
	return f.alerts, f.err
}

func TestSendRetriesThenGivesUp(t *testing.T) {
	flaky := &fakeChannel{name: "telegram", fails: 2}
	down := &fakeChannel{name: "slack", fails: -1}
	var logs bytes.Buffer
	n := &Notifier{Channels: []provider.CommunicationProvider{flaky, down}, Retries: 2, Backoff: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	got := n.Send(context.Background(), provider.TeamMessage{Title: "t"})
	if strings.Join(got, ",") != "telegram" || len(flaky.msgs) != 1 {
		t.Errorf("delivered to %v, telegram got %d", got, len(flaky.msgs))
	}
	// Both ends are logged: what went out and what did not.
	for _, want := range []string{`msg="notification sent" channel=telegram`, `msg="notification not delivered" channel=slack`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

func TestMessageMentionsSignalsAndOpenAlerts(t *testing.T) {
	al := &fakeAlerts{alerts: []provider.Alert{
		{Title: "HighErrorRate checkout", State: "firing"},
		{Title: "old one", State: "resolved"},
	}}
	n := &Notifier{Alerts: al}
	c := Check{
		Rollout:    provider.Rollout{Service: api, Sequence: "7", Rollback: true},
		Checkpoint: 15 * time.Minute,
		Late:       true,
		Report: health.Report{Verdict: deg, Signals: []health.SignalResult{
			{Name: "error_rate", Status: deg, Reason: "5xx 0.20% → 6.00%"},
			{Name: "latency", Status: ok, Reason: "fine"},
		}, Notes: []string{"rollback to revision 5"}},
	}
	m := n.message(context.Background(), c, decision{Event: EventVerdict, Severity: provider.SeverityCritical})
	if m.Title != "prod/api: revision 7 degraded (+15m0s)" || m.Severity != provider.SeverityCritical {
		t.Errorf("title %q severity %s", m.Title, m.Severity)
	}
	for _, want := range []string{"Rollback.", "Late check", "error_rate: degraded, 5xx 0.20% → 6.00%", "Already firing for this service (1): HighErrorRate checkout"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, m.Body)
		}
	}
	if strings.Contains(m.Body, "latency") || strings.Contains(m.Body, "old one") {
		t.Errorf("body mentions healthy signals or resolved alerts:\n%s", m.Body)
	}
	if al.asked != "api" {
		t.Errorf("alerts asked for %q", al.asked)
	}

	al.err = errors.New("alertmanager down")
	m = n.message(context.Background(), c, decision{Event: EventVerdict, Severity: provider.SeverityCritical})
	if !strings.Contains(m.Body, "Open alerts could not be checked: alertmanager down") {
		t.Errorf("alerts failure not mentioned:\n%s", m.Body)
	}
}

// verdictChecker returns the verdicts queued for each checkpoint.
type verdictChecker struct {
	verdicts map[time.Duration]health.Status
}

func (v *verdictChecker) CheckRollout(_ context.Context, r provider.Rollout, opts health.CheckOptions) (health.Report, error) {
	return health.Report{Rollout: r, Outcome: health.OutcomeEvaluated, Verdict: v.verdicts[opts.Checkpoint], Checkpoint: opts.Checkpoint}, nil
}

func TestDaemonNotifiesFailureThenRecovery(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram"}
	al := &fakeAlerts{}
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{0: deg, 5 * time.Minute: ok, 15 * time.Minute: deg, 30 * time.Minute: deg}}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}, Alerts: al}

	failed := provider.Rollout{Service: api, Sequence: "2", State: provider.RolloutFailed, StartedAt: t0, FailedAt: t0.Add(10 * time.Minute)}
	h.source.current = failed
	h.at(10 * time.Minute).handle(failed)
	if al.asked != "api" {
		t.Error("alerts not consulted before the failure's critical")
	}

	done := completed("2", 0, 20*time.Minute)
	h.source.current = done
	h.at(20 * time.Minute).handle(done)
	h.at(25 * time.Minute).due() // +5 OK: recovered

	// Restart: the notification state comes back from the journal, so
	// +15 degraded is a new critical (the reference is the recovered
	// level) and +30 degraded is quiet.
	h.restart()
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{15 * time.Minute: deg, 30 * time.Minute: deg}}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.at(35 * time.Minute).due()
	h.at(50 * time.Minute).due()

	want := strings.Join([]string{
		"[critical] prod/api: revision 2 degraded (rollout failure)",
		"[info] prod/api: recovered, revision 2 now ok (+5m0s)",
		"[critical] prod/api: revision 2 degraded (+15m0s)",
	}, "\n")
	if got := tg.titles(); got != want {
		t.Errorf("messages:\n%s\nwant:\n%s", got, want)
	}
	if n := h.count(KindNotification + " "); n != 3 {
		t.Errorf("%d notification records, want 3", n)
	}
}

func TestNotificationStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram"}
	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{5 * time.Minute: deg}}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.at(time.Minute).handle(r)
	h.at(6 * time.Minute).due()

	// Still degraded at +15 after a restart: the critical was already
	// sent for this rollout, so nothing new.
	h.restart()
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{15 * time.Minute: deg}}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.at(16 * time.Minute).due()
	if got := tg.titles(); got != "[critical] prod/api: revision 2 degraded (+5m0s)" {
		t.Errorf("messages:\n%s", got)
	}
}

// A critical records each channel's reference to the message, and a
// reply threads under it where there is one, quoting the title where
// there is none.
func TestCriticalRefsAndReply(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram", refs: true}
	hook := &fakeChannel{name: "slack"}
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{0: health.StatusDegraded}}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg, hook}}

	failed := provider.Rollout{Service: api, Sequence: "2", State: provider.RolloutFailed, StartedAt: t0, FailedAt: t0.Add(10 * time.Minute)}
	h.source.current = failed
	h.at(10 * time.Minute).handle(failed)
	if h.count(`"delivered":["telegram","slack"],"refs":{"telegram":"1"}`) != 1 {
		t.Fatalf("the critical's refs are not recorded:\n%s", strings.Join(h.records(), "\n"))
	}

	refs := map[string]provider.MessageRef{"telegram": "1"}
	got := h.d.Notifier.Reply(context.Background(), refs, tg.msgs[0].Title, provider.TeamMessage{Title: "Investigation"})
	if len(got) != 2 {
		t.Fatalf("reply delivered to %v", got)
	}
	want := map[*fakeChannel]provider.Reply{
		tg:   {Ref: "1", Title: "prod/api: revision 2 degraded (rollout failure)"},
		hook: {Title: "prod/api: revision 2 degraded (rollout failure)"},
	}
	for ch, w := range want {
		r := ch.msgs[len(ch.msgs)-1].ReplyTo
		if r == nil || *r != w {
			t.Errorf("%s: reply to %+v, want %+v", ch.name, r, w)
		}
	}
}
