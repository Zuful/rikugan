package health

import (
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// The fixtures below replay deploy profiles minute by minute. Time is
// expressed in minutes relative to the rollout's completion: the rollout
// starts at -2 and completes at 0, the before window is [-32, -2), and
// the after window of checkpoint +N is [0, N).

var completed = time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)

func minute(m float64) time.Time { return completed.Add(time.Duration(m * float64(time.Minute))) }

// profile samples f once a minute from the start of the before window
// to the last checkpoint.
func profile(f func(m float64) float64) Series {
	var s Series
	for m := -32.0; m < 31; m++ {
		s = append(s, provider.MetricSample{Time: minute(m), Value: f(m)})
	}
	return s
}

func constant(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// step returns before for minutes < 0 and after from 0 on.
func step(before, after float64) func(float64) float64 {
	return func(m float64) float64 {
		if m < 0 {
			return before
		}
		return after
	}
}

func perMinute(n float64) float64 { return n / 60 }

func rollout() provider.Rollout {
	return provider.Rollout{
		Service:     provider.Service{Name: "api", Namespace: "prod"},
		Revision:    "abc123",
		Sequence:    "7",
		State:       provider.RolloutComplete,
		StartedAt:   minute(-2),
		CompletedAt: minute(0),
	}
}

// healthy is a busy, steady service: 1200 req/min, 0.2% errors, p95
// 120 ms, steady resources, same traffic a week earlier.
func healthy() Signals {
	return Signals{
		RequestRate: profile(constant(perMinute(1200))),
		ErrorRate:   profile(constant(perMinute(1200) * 0.002)),
		LatencyP95:  profile(constant(0.120)),
		CPU:         profile(constant(0.25)),
		Memory:      profile(constant(256 << 20)),
	}
}

func readyInstances(n int) []provider.Instance {
	var out []provider.Instance
	for i := 0; i < n; i++ {
		out = append(out, provider.Instance{Name: "api-abc123-" + string(rune('a'+i)), Revision: "abc123", Created: minute(-1.5), Ready: true})
	}
	return out
}

func inputs(sig Signals, checkpoint float64) Inputs {
	week := healthy()
	week.RequestRate = sig.RequestRate
	return Inputs{
		Rollout:   rollout(),
		Now:       minute(checkpoint).Add(10 * time.Second),
		Current:   sig,
		Seasonal:  &Signals{RequestRate: week.RequestRate},
		Instances: readyInstances(3),
	}
}

func TestEvaluate_Fixtures(t *testing.T) {
	tests := []struct {
		name       string
		in         func() Inputs
		verdict    Verdict
		signals    map[string]Status // only the signals the fixture is about
		noteSubstr string
	}{
		{
			name:    "healthy deploy",
			in:      func() Inputs { return inputs(healthy(), 15) },
			verdict: StatusOK,
		},
		{
			name: "warm-up: p95 and CPU spike for the first 3 minutes, back to normal by +8",
			in: func() Inputs {
				s := healthy()
				warm := func(normal, spike float64) func(float64) float64 {
					return func(m float64) float64 {
						if m >= 0 && m < 3 {
							return spike
						}
						return normal
					}
				}
				s.LatencyP95 = profile(warm(0.120, 0.400))
				s.CPU = profile(warm(0.25, 0.60))
				return inputs(s, 15)
			},
			verdict: StatusOK,
			signals: map[string]Status{SignalLatency: StatusOK, SignalSaturation: StatusOK},
		},
		{
			name: "low traffic: 40 requests per window, 3 errors",
			in: func() Inputs {
				s := healthy()
				s.RequestRate = profile(constant(perMinute(40.0 / 15)))
				s.ErrorRate = profile(constant(perMinute(3.0 / 15)))
				return inputs(s, 15)
			},
			verdict: StatusInsufficientData,
			signals: map[string]Status{SignalLatency: StatusInsufficientData, SignalErrorRate: StatusInsufficientData},
		},
		{
			name:    "medium traffic (~10 req/min), healthy, at +5",
			in:      func() Inputs { return inputs(mediumTraffic(0.005, 0.005), 5) },
			verdict: StatusInsufficientData,
		},
		{
			name:    "medium traffic, healthy, at +15",
			in:      func() Inputs { return inputs(mediumTraffic(0.005, 0.005), 15) },
			verdict: StatusInsufficientData,
		},
		{
			name:    "medium traffic, healthy, at +30",
			in:      func() Inputs { return inputs(mediumTraffic(0.005, 0.005), 30) },
			verdict: StatusOK,
		},
		{
			name:    "medium traffic, 5xx 0.5% to 6%, at +5",
			in:      func() Inputs { return inputs(mediumTraffic(0.005, 0.06), 5) },
			verdict: StatusInsufficientData,
		},
		{
			name:    "medium traffic, 5xx 0.5% to 6%, at +15",
			in:      func() Inputs { return inputs(mediumTraffic(0.005, 0.06), 15) },
			verdict: StatusInsufficientData,
		},
		{
			name: "medium traffic, 5xx 0.5% to 6%, at +30: 18 errors is enough to investigate, not to degrade",
			in:   func() Inputs { return inputs(mediumTraffic(0.005, 0.06), 30) },
			// 300 requests × 6% = 18 errors: ×12 and 6% > 5%, but 18 < 25.
			verdict: StatusInvestigate,
			signals: map[string]Status{SignalErrorRate: StatusInvestigate},
		},
		{
			name: "real regression: 5xx 0.2% to 4%, 60 errors",
			in: func() Inputs {
				s := healthy()
				s.RequestRate = profile(constant(perMinute(100)))
				s.ErrorRate = profile(step(perMinute(100)*0.002, perMinute(100)*0.04))
				return inputs(s, 15)
			},
			verdict: StatusInvestigate,
			signals: map[string]Status{SignalErrorRate: StatusInvestigate},
		},
		{
			name: "frank regression: p95 120 ms to 450 ms",
			in: func() Inputs {
				s := healthy()
				s.LatencyP95 = profile(step(0.120, 0.450))
				return inputs(s, 15)
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalLatency: StatusDegraded},
		},
		{
			name: "crash loop: 2 restarts by +4 on a low-traffic service",
			in: func() Inputs {
				s := healthy()
				s.RequestRate = profile(constant(perMinute(2)))
				s.ErrorRate = profile(constant(0))
				in := inputs(s, 5)
				in.Instances[0].RestartCount = 2
				return in
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalRestarts: StatusDegraded, SignalLatency: StatusInsufficientData},
		},
		{
			name: "one isolated restart, healthy metrics",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances[1].RestartCount = 1
				in.Instances[1].Terminations = []provider.Termination{{Reason: "Error", ExitCode: 1}}
				return in
			},
			verdict: StatusInvestigate,
			signals: map[string]Status{SignalRestarts: StatusInvestigate},
		},
		{
			name: "attested OOM kill, one restart",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances[0].RestartCount = 1
				in.Instances[0].Terminations = []provider.Termination{{Reason: "OOMKilled", ExitCode: 137, OOM: true}}
				return in
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalRestarts: StatusDegraded},
		},
		{
			name: "exit 137 without OOM attestation, one restart",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances[0].RestartCount = 1
				in.Instances[0].Terminations = []provider.Termination{{Reason: "Killed", ExitCode: 137}}
				return in
			},
			verdict: StatusInvestigate,
		},
		{
			name: "compose: OOM then successful restart, events gone (Unknown termination)",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances[0].RestartCount = 1
				in.Instances[0].Terminations = []provider.Termination{{Reason: "Unknown", ExitCode: -1}}
				return in
			},
			verdict: StatusInvestigate,
		},
		{
			name: "HPA scale-out: CPU per replica steady, 2 instances added at +4 still starting at +5",
			in: func() Inputs {
				in := inputs(healthy(), 5)
				for _, n := range []string{"y", "z"} {
					in.Instances = append(in.Instances, provider.Instance{
						Name: "api-abc123-" + n, Created: minute(4), NotReadySince: minute(4),
					})
				}
				return in
			},
			verdict: StatusOK,
			signals: map[string]Status{SignalAvailability: StatusOK},
		},
		{
			name: "instance added after the rollout, not ready for 5 minutes",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances = append(in.Instances, provider.Instance{
					Name: "api-abc123-y", Created: minute(9), NotReadySince: minute(10),
				})
				return in
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalAvailability: StatusDegraded},
		},
		{
			name: "rollout instance not ready for 30 s",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances[0].Ready = false
				in.Instances[0].NotReadySince = in.Now.Add(-30 * time.Second)
				return in
			},
			verdict: StatusOK,
			signals: map[string]Status{SignalAvailability: StatusOK},
		},
		{
			name: "rollout instance not ready for 90 s",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Instances[0].Ready = false
				in.Instances[0].NotReadySince = in.Now.Add(-90 * time.Second)
				return in
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalAvailability: StatusDegraded},
		},
		{
			name: "evening deploy: traffic -55% vs before, stable vs a week earlier",
			in: func() Inputs {
				s := healthy()
				s.RequestRate = profile(step(perMinute(1200), perMinute(540)))
				s.ErrorRate = profile(step(perMinute(1200)*0.002, perMinute(540)*0.002))
				in := inputs(s, 15)
				in.Seasonal = &Signals{RequestRate: profile(constant(perMinute(560)))}
				return in
			},
			verdict: StatusOK,
			signals: map[string]Status{SignalTraffic: StatusOK},
		},
		{
			name: "traffic -55% vs before and vs a week earlier",
			in: func() Inputs {
				s := healthy()
				s.RequestRate = profile(step(perMinute(1200), perMinute(540)))
				s.ErrorRate = profile(step(perMinute(1200)*0.002, perMinute(540)*0.002))
				in := inputs(s, 15)
				in.Seasonal = &Signals{RequestRate: profile(constant(perMinute(1200)))}
				return in
			},
			verdict: StatusInvestigate,
			signals: map[string]Status{SignalTraffic: StatusInvestigate},
		},
		{
			name: "traffic collapses to zero: the before window alone is enough",
			in: func() Inputs {
				s := healthy()
				s.RequestRate = profile(step(perMinute(1200), perMinute(1200)*0.01))
				in := inputs(s, 15)
				// A week earlier was just as quiet (a holiday, say): on
				// the two-baseline rule this would pass.
				in.Seasonal = &Signals{RequestRate: profile(constant(perMinute(1200) * 0.01))}
				return in
			},
			signals: map[string]Status{SignalTraffic: StatusInvestigate},
		},
		{
			name: "recent service without data a week earlier",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Seasonal = nil
				return in
			},
			verdict:    StatusOK,
			noteSubstr: "seasonality guard disabled",
		},
		{
			name: "rollback is evaluated like any rollout",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Rollout.Rollback = true
				return in
			},
			verdict:    StatusOK,
			noteSubstr: "rollback to revision 7",
		},
		{
			name: "stalled rollout past its progress deadline",
			in: func() Inputs {
				in := inputs(healthy(), 15)
				in.Rollout.State = provider.RolloutFailed
				in.Rollout.CompletedAt = time.Time{}
				in.Rollout.FailedAt = minute(8)
				in.Rollout.FailureReason = `ProgressDeadlineExceeded: ReplicaSet "api-abc123" has timed out progressing.`
				return in
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalRollout: StatusDegraded},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(DefaultConfig(), tc.in())
			if r.Outcome != OutcomeEvaluated {
				t.Fatalf("outcome %s, want evaluated (notes %v)", r.Outcome, r.Notes)
			}
			if tc.verdict != "" && r.Verdict != tc.verdict {
				t.Errorf("verdict %s, want %s\n%s", r.Verdict, tc.verdict, dump(r))
			}
			for name, want := range tc.signals {
				got, ok := r.Signal(name)
				if !ok || got.Status != want {
					t.Errorf("signal %s: %+v, want %s\n%s", name, got, want, dump(r))
				}
			}
			if tc.noteSubstr != "" && !strings.Contains(strings.Join(r.Notes, "\n"), tc.noteSubstr) {
				t.Errorf("notes %q lack %q", r.Notes, tc.noteSubstr)
			}
		})
	}
}

// mediumTraffic is ~10 requests a minute (300 over 30 minutes), with
// the given 5xx share before and after the rollout.
func mediumTraffic(errBefore, errAfter float64) Signals {
	s := healthy()
	s.RequestRate = profile(constant(perMinute(10)))
	s.ErrorRate = profile(step(perMinute(10)*errBefore, perMinute(10)*errAfter))
	return s
}

func TestEvaluate_WarmUpAtFirstCheckpoint(t *testing.T) {
	// The same warm-up as the +15 fixture, read at +5: three of the five
	// minutes are still warming up, so the median p95 is the spike.
	// Saturation is skipped at +5, latency is not.
	s := healthy()
	s.LatencyP95 = profile(func(m float64) float64 {
		if m >= 0 && m < 3 {
			return 0.400
		}
		return 0.120
	})
	r := Evaluate(DefaultConfig(), inputs(s, 5))
	if got, _ := r.Signal(SignalLatency); got.Status != StatusDegraded {
		t.Errorf("latency at +5 during warm-up: %+v", got)
	}
	if got, _ := r.Signal(SignalSaturation); got.Status != StatusNotEvaluated {
		t.Errorf("saturation must not be evaluated at +5: %+v", got)
	}
}

func TestEvaluate_NoVerdictOutcomes(t *testing.T) {
	cfg := DefaultConfig()

	t.Run("rollout in progress reports instance signals", func(t *testing.T) {
		in := inputs(healthy(), 15)
		in.Rollout.State = provider.RolloutInProgress
		in.Rollout.CompletedAt = time.Time{}
		in.Instances[0].RestartCount = 1
		r := Evaluate(cfg, in)
		if r.Outcome != OutcomeInProgress || r.Verdict != "" {
			t.Fatalf("got outcome %s verdict %q", r.Outcome, r.Verdict)
		}
		if got, _ := r.Signal(SignalRestarts); got.Status != StatusInvestigate {
			t.Errorf("restarts of the new revision not reported: %+v", got)
		}
		if !strings.Contains(strings.Join(r.Notes, ""), "rollout in progress for") {
			t.Errorf("notes %q", r.Notes)
		}
	})

	t.Run("too early", func(t *testing.T) {
		in := inputs(healthy(), 3)
		r := Evaluate(cfg, in)
		if r.Outcome != OutcomeTooEarly || r.Verdict != "" || !r.NextCheck.Equal(minute(5)) {
			t.Fatalf("got outcome %s verdict %q next %s", r.Outcome, r.Verdict, r.NextCheck)
		}
	})

	t.Run("stale", func(t *testing.T) {
		in := inputs(healthy(), 15)
		in.Now = minute(121)
		r := Evaluate(cfg, in)
		if r.Outcome != OutcomeStale || r.Verdict != "" {
			t.Fatalf("got outcome %s verdict %q", r.Outcome, r.Verdict)
		}
	})
}

func TestSelectCheckpoint(t *testing.T) {
	cfg := DefaultConfig()
	for _, tc := range []struct {
		elapsed time.Duration
		cp      time.Duration
		outcome Outcome
	}{
		{4 * time.Minute, 0, OutcomeTooEarly},
		{5 * time.Minute, 5 * time.Minute, OutcomeEvaluated},
		{14 * time.Minute, 5 * time.Minute, OutcomeEvaluated},
		{15 * time.Minute, 15 * time.Minute, OutcomeEvaluated},
		{90 * time.Minute, 30 * time.Minute, OutcomeEvaluated},
		{121 * time.Minute, 0, OutcomeStale},
	} {
		cp, out := SelectCheckpoint(cfg, completed, completed.Add(tc.elapsed))
		if cp != tc.cp || out != tc.outcome {
			t.Errorf("elapsed %s: got %s/%s, want %s/%s", tc.elapsed, cp, out, tc.cp, tc.outcome)
		}
	}
}

func dump(r Report) string {
	var b strings.Builder
	for _, s := range r.Signals {
		b.WriteString("  " + s.Name + ": " + string(s.Status) + " — " + s.Reason + "\n")
	}
	return b.String()
}

func TestStatusSeverity(t *testing.T) {
	for status, want := range map[Status]provider.Severity{
		StatusDegraded:         provider.SeverityCritical,
		StatusInvestigate:      provider.SeverityWarning,
		StatusOK:               provider.SeverityInfo,
		StatusInsufficientData: provider.SeverityInfo,
	} {
		if got := status.Severity(); got != want {
			t.Errorf("%s: %s, want %s", status, got, want)
		}
	}
}
