package watch

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

var promDown = &health.BackendError{Kind: health.BackendMetrics, Name: "prometheus", Err: errors.New("connection refused")}

// Prometheus down for hours: the checkpoints are retried, then given up
// past stale_after and recorded as not evaluated, with one warning for
// the outage; the first check that reads metrics again closes it.
func TestMetricsOutageIsGivenUpAndWarnedOnce(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram"}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.checker.verdict, h.checker.err = health.StatusInsufficientData, promDown

	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	for m := 6; m <= 120; m++ {
		h.at(time.Duration(m) * time.Minute).due()
	}
	if n := h.count(KindCheck + " "); n != 0 {
		t.Errorf("%d check records while metrics are down, want none", n)
	}
	if got := tg.titles(); got != "" {
		t.Errorf("warned before stale_after: %q", got)
	}

	// Past stale_after: given up, warned once.
	h.at(122 * time.Minute).due()
	if n := h.count("not evaluated, given up past stale_after: metrics backend prometheus: connection refused"); n != 1 {
		t.Errorf("%d checkpoints given up past stale_after, want +30", n)
	}
	if n := h.count("not evaluated, the next checkpoint came while checks were failing: metrics backend prometheus"); n != 2 {
		t.Errorf("%d checkpoints passed over while failing, want +5 and +15\n%s", n, strings.Join(h.records(), "\n"))
	}
	// The outage started at +5 (12:06), under stale_after ago: the give-up
	// itself is what warns.
	if got := tg.titles(); got != "[warning] rikugan cannot evaluate: metrics backend prometheus failing since 2026-10-18 12:06Z" {
		t.Errorf("no warning when the checks were given up:\n%s", got)
	}
	r3 := completed("3", 121*time.Minute, 122*time.Minute)
	h.source.current = r3
	h.handle(r3)
	h.at(127 * time.Minute).due()
	h.at(140 * time.Minute).due()
	if got := tg.titles(); got != "[warning] rikugan cannot evaluate: metrics backend prometheus failing since 2026-10-18 12:06Z" {
		t.Errorf("messages:\n%s", got)
	}

	// Back: the next check closes the outage.
	h.checker.verdict, h.checker.err = "", nil
	h.at(141 * time.Minute).due()
	want := "[warning] rikugan cannot evaluate: metrics backend prometheus failing since 2026-10-18 12:06Z\n" +
		"[info] rikugan evaluates again: metrics backend prometheus back after 2h15m0s"
	if got := tg.titles(); got != want {
		t.Errorf("messages:\n%s\nwant:\n%s", got, want)
	}
}

// Metrics down but instances read: a degraded verdict on the instance
// signals is notified at once, recorded as partial, and the checkpoint
// is still checked in full once the metrics are back, without a second
// critical.
func TestPartialDegradedIsNotifiedWithoutWaitingForMetrics(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram"}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.checker.verdict, h.checker.err = health.StatusDegraded, promDown

	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	h.at(6 * time.Minute).due()
	h.at(7 * time.Minute).due() // still down: retried, nothing new
	if got := tg.titles(); got != "[critical] prod/api: revision 2 degraded (+5m0s)" {
		t.Errorf("messages:\n%s", got)
	}
	if n := h.count(`"partial":true`); n != 1 {
		t.Errorf("%d partial check records, want 1", n)
	}

	// A restart does not take the partial check for a full one.
	h.restart()
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.checker.err = nil
	h.at(8 * time.Minute).due()
	if n := h.count(KindCheck + " "); n != 2 {
		t.Errorf("%d check records, want the partial one and the full one", n)
	}
	if n := len(strings.Split(tg.titles(), "\n")); n != 1 {
		t.Errorf("a second critical for the same rollout:\n%s", tg.titles())
	}
}

// The immediate check of a failed rollout is retried after a backend
// error, as checkpoints are.
func TestFailedRolloutCheckIsRetried(t *testing.T) {
	h := newHarness(t)
	failed := provider.Rollout{Service: api, Sequence: "2", State: provider.RolloutFailed, StartedAt: t0, FailedAt: t0.Add(10 * time.Minute)}
	h.source.current = failed
	h.checker.fail = true
	h.at(10 * time.Minute).handle(failed)
	if next := h.due(); !next.Equal(t0.Add(11 * time.Minute)) {
		t.Errorf("retry at %s, want a minute later", next)
	}
	h.checker.fail = false
	h.at(11 * time.Minute).due()
	if got := h.checker.seen(); got != "2@0s" {
		t.Errorf("checks %q", got)
	}
	if next := h.at(12 * time.Minute).due(); !next.IsZero() {
		t.Errorf("nothing left to do, next %s", next)
	}
}

// No channel takes a critical: nothing is recorded, and the check is
// run again until a channel takes it.
func TestUndeliveredNotificationIsRetried(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram", fails: -1}
	sl := &fakeChannel{name: "slack", fails: -1}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg, sl}}
	h.checker.verdict = health.StatusDegraded

	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	if next := h.at(6 * time.Minute).due(); !next.Equal(t0.Add(7 * time.Minute)) {
		t.Errorf("retry at %s, want a minute later", next)
	}
	if n := h.count(KindCheck+" ") + h.count(KindNotification+" "); n != 0 {
		t.Errorf("%d records while no channel took the message, want none", n)
	}
	sl.mu.Lock()
	sl.fails = 0
	sl.mu.Unlock()
	h.at(7 * time.Minute).due()
	if got := sl.titles(); got != "[critical] prod/api: revision 2 degraded (+5m0s)" {
		t.Errorf("slack:\n%s", got)
	}
	if h.count(KindCheck+" ") != 1 || h.count(`"delivered":["slack"]`) != 1 {
		t.Errorf("records:\n%s", strings.Join(h.records(), "\n"))
	}
}

func TestUndeliveredSweepNotificationIsRetried(t *testing.T) {
	h, chk, tg := sweepHarness(t, map[string][]health.Status{"shop/api": {health.StatusDegraded, health.StatusDegraded}})
	tg.fails = -1
	h.d.Sweep.Namespaces = []string{"shop"}
	next := h.due()
	if !next.Equal(t0.Add(time.Minute)) {
		t.Errorf("next %s, want the retry a minute later", next)
	}
	if n := h.count(KindSweep + " "); n != 1 {
		t.Errorf("%d sweep records, want shop/web only: no channel took shop/api's critical", n)
	}
	tg.mu.Lock()
	tg.fails = 0
	tg.mu.Unlock()
	h.at(time.Minute).due()
	if got := tg.titles(); got != "[critical] shop/api: degraded (periodic sweep)" {
		t.Errorf("messages:\n%s", got)
	}
	if got := len(chk.prevs["shop/api"]); got != 2 {
		t.Errorf("shop/api checked %d times, want the sweep and its retry", got)
	}
	if chk.prevs["shop/api"][1] != nil {
		t.Errorf("the retry must use the same previous sweep (none), got %+v", chk.prevs["shop/api"][1])
	}
	if n := h.count(KindSweep + " "); n != 2 {
		t.Errorf("%d sweep records after the retry, want 2", n)
	}
}

// A sweep with the metrics backend failing still evaluates and records
// the instance signals.
func TestSweepWithMetricsDownStillRecords(t *testing.T) {
	h, chk, _ := sweepHarness(t, map[string][]health.Status{"shop/api": {health.StatusDegraded}})
	chk.err = promDown
	h.due()
	if n := h.count(KindSweep + " "); n != 2 {
		t.Errorf("%d sweep records, want shop/api and shop/web", n)
	}
}
