package sweep

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Zuful/rikugan/internal/health"
)

const (
	ok  = health.StatusOK
	inv = health.StatusInvestigate
	deg = health.StatusDegraded
	ins = health.StatusInsufficientData
)

// run replays verdicts from the zero state and returns one entry per
// round: the notification event, or "-".
func run(cfg Config, verdicts ...health.Verdict) (State, string) {
	var s State
	var out []string
	for _, v := range verdicts {
		var n *Notification
		s, n = Step(s, v, cfg)
		if n == nil {
			out = append(out, "-")
		} else {
			out = append(out, fmt.Sprintf("%s/%s", n.Event, n.Severity))
		}
	}
	return s, strings.Join(out, " ")
}

// The five scenarios the design lists for this state machine (§6).
func TestDesignScenarios(t *testing.T) {
	tests := []struct {
		name     string
		verdicts []health.Verdict
		want     string
	}{
		{
			name:     "leak on 3 consecutive rounds: one warning, then nothing on rounds 4 and 5",
			verdicts: []health.Verdict{inv, inv, inv, inv, inv},
			want:     "- - persistent_warning/warning - -",
		},
		{
			name:     "investigate x2, OK, investigate x2: the series is broken, no notification",
			verdicts: []health.Verdict{inv, inv, ok, inv, inv},
			want:     "- - - - -",
		},
		{
			name:     "investigate x2, insufficient data, investigate: notified on round 4",
			verdicts: []health.Verdict{inv, inv, ins, inv},
			want:     "- - - persistent_warning/warning",
		},
		{
			name:     "warning then degraded: critical, then nothing while the incident lasts",
			verdicts: []health.Verdict{inv, deg, deg, inv, deg},
			want:     "- incident/critical - - -",
		},
		{
			name:     "degraded, OK, degraded before the second OK: a single critical",
			verdicts: []health.Verdict{deg, ok, deg},
			want:     "incident/critical - -",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := run(Config{}, tt.verdicts...); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// Every row of the design's transition table.
func TestTransitions(t *testing.T) {
	tests := []struct {
		name      string
		verdicts  []health.Verdict
		wantPhase Phase
		want      string
	}{
		{"ok stays ok", []health.Verdict{ok, ok}, PhaseOK, "- -"},
		{"ok to incident", []health.Verdict{deg}, PhaseIncident, "incident/critical"},
		{"ok to warning, silent by default", []health.Verdict{inv}, PhaseWarning, "-"},
		{"warning back to ok, silent", []health.Verdict{inv, ok}, PhaseOK, "- -"},
		{"warning to incident", []health.Verdict{inv, inv, deg}, PhaseIncident, "- - incident/critical"},
		{"incident lasts on degraded", []health.Verdict{deg, deg}, PhaseIncident, "incident/critical -"},
		{"incident stays on investigate", []health.Verdict{deg, inv}, PhaseIncident, "incident/critical -"},
		{"incident to resolving on ok", []health.Verdict{deg, ok}, PhaseResolving, "incident/critical -"},
		{"resolved after two ok rounds", []health.Verdict{deg, ok, ok}, PhaseOK, "incident/critical - resolved/info"},
		{
			"investigate while resolving restarts the ok count",
			[]health.Verdict{deg, ok, inv, ok}, PhaseResolving, "incident/critical - - -",
		},
		{
			"resolved needs two consecutive ok rounds after an investigate",
			[]health.Verdict{deg, ok, inv, ok, ok}, PhaseOK, "incident/critical - - - resolved/info",
		},
		{"resolving back to incident, silent", []health.Verdict{deg, ok, deg}, PhaseIncident, "incident/critical - -"},
		{
			"insufficient data does not count towards resolution",
			[]health.Verdict{deg, ok, ins, ok}, PhaseOK, "incident/critical - - resolved/info",
		},
		{"insufficient data keeps an incident", []health.Verdict{deg, ins}, PhaseIncident, "incident/critical -"},
		{"insufficient data from ok stays ok", []health.Verdict{ins}, PhaseOK, "-"},
		{
			"a new incident after a confirmed recovery is notified again",
			[]health.Verdict{deg, ok, ok, deg}, PhaseIncident, "incident/critical - resolved/info incident/critical",
		},
		{
			"a new series after a broken one can notify again",
			[]health.Verdict{inv, inv, inv, ok, inv, inv, inv}, PhaseWarning,
			"- - persistent_warning/warning - - - persistent_warning/warning",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, got := run(Config{}, tt.verdicts...)
			if got != tt.want || s.phase() != tt.wantPhase {
				t.Errorf("got %s in %s\nwant %s in %s", got, s.phase(), tt.want, tt.wantPhase)
			}
		})
	}
}

func TestNotifyWarningAndPersistence(t *testing.T) {
	if _, got := run(Config{NotifyWarning: true}, inv, inv, inv, inv); got != "warning/warning - persistent_warning/warning -" {
		t.Errorf("notify_warning: %s", got)
	}
	if _, got := run(Config{WarningPersistence: 2}, inv, inv, inv); got != "- persistent_warning/warning -" {
		t.Errorf("persistence 2: %s", got)
	}
	if _, got := run(Config{WarningPersistence: 1, NotifyWarning: true}, inv, inv); got != "persistent_warning/warning -" {
		t.Errorf("persistence 1: %s", got)
	}
	s, _ := run(Config{}, inv, inv, inv)
	if s.Streak != 3 || !s.WarningNotified {
		t.Errorf("state after 3 rounds: %+v", s)
	}
}

func TestPersistentWarningCarriesStreak(t *testing.T) {
	var s State
	var n *Notification
	for range 3 {
		s, n = Step(s, inv, Config{})
	}
	if n == nil || n.Streak != 3 {
		t.Errorf("notification %+v, want streak 3", n)
	}
}
