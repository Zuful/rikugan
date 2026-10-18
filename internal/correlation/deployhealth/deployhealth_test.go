package deployhealth

import (
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

var t0 = time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)

func rollout(state provider.RolloutState) provider.Rollout {
	return provider.Rollout{
		Service:  provider.Service{Name: "api", Namespace: "prod"},
		Revision: "abc123", Sequence: "7", Images: []string{"api:1.4.2"},
		State: state, StartedAt: t0,
	}
}

func TestRender(t *testing.T) {
	complete := rollout(provider.RolloutComplete)
	complete.CompletedAt = t0.Add(2 * time.Minute)
	failed := rollout(provider.RolloutFailed)
	failed.FailedAt = t0.Add(10 * time.Minute)

	tests := []struct {
		name   string
		report health.Report
		want   []string
	}{
		{
			name: "evaluated",
			report: health.Report{
				Rollout: complete, Outcome: health.OutcomeEvaluated, Verdict: health.StatusDegraded, Checkpoint: 15 * time.Minute,
				Signals: []health.SignalResult{{Name: health.SignalLatency, Status: health.StatusDegraded, Reason: "p95 120ms → 450ms"}},
			},
			want: []string{"`prod/api`", "revision 7 (`abc123`)", "`api:1.4.2`", "Verdict: **DEGRADED** (checkpoint +15m0s)", "| latency | degraded | p95 120ms → 450ms |"},
		},
		{
			name:   "stalled rollout",
			report: health.Report{Rollout: failed, Outcome: health.OutcomeEvaluated, Verdict: health.StatusDegraded},
			want:   []string{"**Failed:** 2026-10-18 14:10:00Z", "Verdict: **DEGRADED**"},
		},
		{
			name:   "in progress",
			report: health.Report{Rollout: rollout(provider.RolloutInProgress), Outcome: health.OutcomeInProgress, Notes: []string{"rollout in progress for 3m0s"}},
			want:   []string{"**In progress**", "No verdict: rollout in progress", "- rollout in progress for 3m0s"},
		},
		{
			name:   "too early",
			report: health.Report{Rollout: complete, Outcome: health.OutcomeTooEarly, NextCheck: t0.Add(7 * time.Minute)},
			want:   []string{"first check possible at 2026-10-18 14:07:00Z"},
		},
		{
			name:   "stale",
			report: health.Report{Rollout: complete, Outcome: health.OutcomeStale},
			want:   []string{"too old", "`compare_to_baseline`"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := Render(tc.report)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
		})
	}
}
