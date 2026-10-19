package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var run0 = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return run0.Add(d) }

func rec(t *testing.T, when time.Duration, kind, service string, data any) record {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return record{Time: at(when), Kind: kind, Service: "lab-shop/" + service, Data: raw}
}

func sigs(pairs ...string) []signal {
	var out []signal
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, signal{Name: pairs[i], Status: pairs[i+1]})
	}
	return out
}

type checkData struct {
	Sequence   string   `json:"sequence"`
	Checkpoint string   `json:"checkpoint"`
	Outcome    string   `json:"outcome"`
	Verdict    string   `json:"verdict"`
	Signals    []signal `json:"signals"`
}

func check(seq, cp, verdict string, s []signal) checkData {
	return checkData{Sequence: seq, Checkpoint: cp, Outcome: "evaluated", Verdict: verdict, Signals: s}
}

type rolloutData struct {
	Sequence  string    `json:"sequence"`
	StartedAt time.Time `json:"started_at"`
}

type noteData struct {
	Sequence string `json:"sequence"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
}

type sweepData struct {
	Signals  []signal `json:"signals"`
	Notified *struct {
		Severity string `json:"severity"`
		Title    string `json:"title"`
	} `json:"notified,omitempty"`
}

func sweepNotified(severity, title string, s []signal) sweepData {
	d := sweepData{Signals: s}
	d.Notified = &struct {
		Severity string `json:"severity"`
		Title    string `json:"title"`
	}{severity, title}
	return d
}

func deploy(when time.Duration, svc, scenario, severity string, signals ...string) action {
	return action{Time: at(when), Action: "deploy", Service: svc, Scenario: scenario, Severity: severity, Signals: signals, Services: []string{svc}}
}

func TestScoreRun(t *testing.T) {
	h := time.Hour
	m := time.Minute
	actions := []action{
		{Time: at(0), Action: "up", Severity: "none"},
		deploy(1*h, "web", "errors", "degraded", "error_rate"),
		deploy(2*h, "web", "healthy", "none"),
		deploy(3*h, "low", "minor-errors", "investigate", "error_rate"),
		{Time: at(4 * h), Action: "dependency-stop", Service: "db", Severity: "degraded", Signals: []string{"error_rate"}, Services: []string{"web", "api"}},
		{Time: at(5 * h), Action: "dependency-start", Service: "db", Severity: "none", Services: []string{"web", "api"}},
		deploy(6*h, "web", "slow", "degraded", "latency"),
		deploy(7*h, "worker", "crash", "degraded", "restarts", "availability"),
	}
	records := []record{
		// web errors: detected at +5, critical at +5m30s.
		rec(t, 1*h+2*time.Second, "rollout", "web", rolloutData{"a1", at(1*h + 2*time.Second)}),
		rec(t, 1*h+5*m, "check", "web", check("a1", "5m0s", "degraded", sigs("error_rate", "degraded", "latency", "ok"))),
		rec(t, 1*h+5*m+30*time.Second, "notification", "web", noteData{"a1", "critical", "shop/web: degraded"}),
		rec(t, 1*h+15*m, "check", "web", check("a1", "15m0s", "degraded", sigs("error_rate", "degraded", "latency", "ok"))),
		// web healthy: a false positive on latency at +5, not notified.
		rec(t, 2*h+3*time.Second, "rollout", "web", rolloutData{"b2", at(2*h + 3*time.Second)}),
		rec(t, 2*h+5*m, "check", "web", check("b2", "5m0s", "investigate", sigs("error_rate", "ok", "latency", "investigate"))),
		rec(t, 2*h+15*m, "check", "web", check("b2", "15m0s", "ok", sigs("error_rate", "ok", "latency", "ok"))),
		// low minor errors: too few requests, missed.
		rec(t, 3*h+2*time.Second, "rollout", "low", rolloutData{"c3", at(3*h + 2*time.Second)}),
		rec(t, 3*h+5*m, "check", "low", check("c3", "5m0s", "insufficient_data", sigs("error_rate", "not_evaluated", "latency", "not_evaluated"))),
		rec(t, 3*h+15*m, "check", "low", check("c3", "15m0s", "insufficient_data", sigs("error_rate", "not_evaluated", "latency", "not_evaluated"))),
		rec(t, 3*h+30*m, "check", "low", check("c3", "30m0s", "ok", sigs("error_rate", "ok", "latency", "ok"))),
		// db down: both sweeps degraded, one grouped critical each.
		rec(t, 4*h+30*m, "sweep", "web", sweepNotified("critical", "2 services degraded (periodic sweep)", sigs("error_rate", "degraded"))),
		rec(t, 4*h+30*m, "sweep", "api", sweepNotified("critical", "2 services degraded (periodic sweep)", sigs("error_rate", "degraded"))),
		// web slow: degraded from +15, critical at +16 min (allowed 20).
		rec(t, 6*h+2*time.Second, "rollout", "web", rolloutData{"d4", at(6*h + 2*time.Second)}),
		rec(t, 6*h+5*m, "check", "web", check("d4", "5m0s", "investigate", sigs("latency", "investigate"))),
		rec(t, 6*h+16*m, "notification", "web", noteData{"d4", "critical", "shop/web: degraded"}),
		rec(t, 6*h+16*m, "check", "web", check("d4", "15m0s", "degraded", sigs("latency", "degraded"))),
		// worker crash: failed at the deadline.
		rec(t, 7*h+2*time.Second, "rollout", "worker", rolloutData{"e5", at(7*h + 2*time.Second)}),
		rec(t, 7*h+5*m, "check", "worker", check("e5", "0s", "degraded", sigs("restarts", "degraded"))),
		rec(t, 7*h+5*m+10*time.Second, "notification", "worker", noteData{"e5", "critical", "shop/worker: rollout failed"}),
		// api: a critical out of nowhere.
		rec(t, 10*h, "sweep", "api", sweepNotified("critical", "shop/api: degraded (periodic sweep)", sigs("traffic", "degraded"))),
		rec(t, 12*h, "sweep", "web", sweepData{Signals: sigs("error_rate", "ok")}),
	}
	s := scoreRun(actions, records)

	type row struct {
		detected bool
		reached  string
		delay    time.Duration // 0: never notified
	}
	got := map[string]row{}
	for _, in := range s.injections {
		var delay time.Duration
		if !in.firstNote.IsZero() {
			delay = in.firstNote.Sub(in.start)
		}
		got[in.service+" "+in.scenario] = row{in.detected, in.reached, delay}
	}
	want := map[string]row{
		"web errors":          {true, "degraded", 5*m + 30*time.Second},
		"low minor-errors":    {false, "", 0},
		"web dependency down": {true, "degraded", 30 * m},
		"api dependency down": {true, "degraded", 30 * m},
		"web slow":            {true, "degraded", 16 * m},
		"worker crash":        {true, "degraded", 5*m + 10*time.Second},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %+v, want %+v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("injections %v", got)
	}

	if c := s.cells["5m0s"]["latency"]; c == nil || c.fp != 1 || c.tp != 1 {
		t.Errorf("latency at +5: %+v, want the healthy rollout's FP and the slow one's TP", c)
	}
	if c := s.cells["5m0s"]["error_rate"]; c == nil || c.tp != 1 || c.notEvaluated != 1 || c.tn != 1 {
		t.Errorf("error rate at +5: %+v", c)
	}
	if c := s.cells["30m0s"]["error_rate"]; c == nil || c.fn != 1 {
		t.Errorf("error rate at +30: %+v, want low's miss", c)
	}
	if sd := s.services["low"]; sd.checks != 3 || sd.insufficient != 2 || sd.errorRateUnevaluated != 2 {
		t.Errorf("low: %+v", sd)
	}

	var report bytes.Buffer
	s.writeReport(&report)
	out := report.String()
	for _, line := range []string{
		"**FAIL** no critical without a regression behind it: 1 (and 0 warnings)",
		"under the 72h0m0s needed to conclude",
		"**PASS** every frank regression detected: 5 of 5.",
		"**PASS** every frank regression deployed notified in time",
		"| low | 3 | 67 % | 67 % | 67 % |",
		"shop/api: degraded (periodic sweep) (sweep)",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("report lacks %q:\n%s", line, out)
		}
	}
	var notes bytes.Buffer
	if err := s.writeNotifications(&notes); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(notes.String(), "\n"); lines != 7 {
		t.Errorf("notifications.csv has %d lines, want a header and 6:\n%s", lines, notes.String())
	}
	if !strings.Contains(notes.String(), "shop/api: degraded (periodic sweep),false,") {
		t.Errorf("the unjustified critical is not marked:\n%s", notes.String())
	}
}

// A frank error regression notified at +12 min is late.
func TestScoreLateNotification(t *testing.T) {
	actions := []action{deploy(0, "web", "errors", "degraded", "error_rate")}
	records := []record{
		rec(t, 2*time.Second, "rollout", "web", rolloutData{"a1", at(2 * time.Second)}),
		rec(t, 12*time.Minute, "check", "web", check("a1", "5m0s", "degraded", sigs("error_rate", "degraded"))),
		rec(t, 12*time.Minute, "notification", "web", noteData{"a1", "critical", "late"}),
	}
	var report bytes.Buffer
	scoreRun(actions, records).writeReport(&report)
	if !strings.Contains(report.String(), "**FAIL** every frank regression deployed notified in time (10m0s, 20m0s for latency): 1 late or never.") {
		t.Errorf("report:\n%s", report.String())
	}
}
