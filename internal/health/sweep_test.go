package health

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

var sweepNow = time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)

// ago builds a per-minute series over the last 7 hours from f(hours ago).
func ago(f func(h float64) float64) Series {
	var s Series
	for m := 7 * 60; m >= 0; m-- {
		at := sweepNow.Add(-time.Duration(m) * time.Minute)
		s = append(s, provider.MetricSample{Time: at, Value: f(float64(m) / 60)})
	}
	return s
}

func flat(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// lastHour keeps only the last hour of a series: the shape of a
// baseline, already shifted to line up with now.
func lastHour(s Series) Series {
	var out Series
	for _, p := range s {
		if !p.Time.Before(sweepNow.Add(-time.Hour)) {
			out = append(out, p)
		}
	}
	return out
}

const mb = 1 << 20

func steady() Signals {
	return Signals{
		RequestRate: ago(flat(perMinute(1200))),
		ErrorRate:   ago(flat(perMinute(1200) * 0.002)),
		LatencyP95:  ago(flat(0.120)),
		CPU:         ago(flat(0.25)),
		Memory:      ago(flat(400 * mb)),
	}
}

func baselineOf(s Signals) *Signals {
	return &Signals{
		RequestRate: lastHour(s.RequestRate), ErrorRate: lastHour(s.ErrorRate),
		LatencyP95: lastHour(s.LatencyP95), CPU: lastHour(s.CPU), Memory: lastHour(s.Memory),
	}
}

func sweepIn(current Signals, baseline *Signals) SweepInputs {
	return SweepInputs{
		Now: sweepNow, Current: current, Baseline: baseline, BaselineAge: 7 * 24 * time.Hour,
		Instances: readyInstances(3),
	}
}

func TestEvaluateSweep(t *testing.T) {
	tests := []struct {
		name       string
		in         func() SweepInputs
		verdict    Verdict
		signals    map[string]Status
		noteSubstr string
	}{
		{
			name:       "steady service, same as one week earlier",
			in:         func() SweepInputs { return sweepIn(steady(), baselineOf(steady())) },
			verdict:    StatusOK,
			signals:    map[string]Status{SignalLatency: StatusOK, SignalErrorRate: StatusOK, SignalTraffic: StatusOK, SignalMemoryLeak: StatusOK},
			noteSubstr: "the same hour one week earlier",
		},
		{
			name: "no request metric at all, error query padded with vector(0): error rate never 0%",
			in: func() SweepInputs {
				s := steady()
				s.RequestRate = nil
				s.ErrorRate = ago(flat(0))
				return sweepIn(s, baselineOf(s))
			},
			verdict: StatusInsufficientData,
			signals: map[string]Status{SignalErrorRate: StatusInsufficientData},
		},
		{
			name: "a dependency went down: 5xx 0.2% one week earlier, 8% in the last hour",
			in: func() SweepInputs {
				cur := steady()
				cur.ErrorRate = ago(func(h float64) float64 {
					if h < 1 {
						return perMinute(1200) * 0.08
					}
					return perMinute(1200) * 0.002
				})
				return sweepIn(cur, baselineOf(steady()))
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalErrorRate: StatusDegraded},
		},
		{
			name: "slow degradation: p95 120 ms one week earlier, 450 ms now (no warm-up cap)",
			in: func() SweepInputs {
				cur := steady()
				cur.LatencyP95 = ago(flat(0.450))
				return sweepIn(cur, baselineOf(steady()))
			},
			verdict: StatusDegraded,
			signals: map[string]Status{SignalLatency: StatusDegraded},
		},
		{
			name: "traffic collapse towards zero",
			in: func() SweepInputs {
				cur := steady()
				cur.RequestRate = ago(func(h float64) float64 {
					if h < 1 {
						return perMinute(1200) * 0.01
					}
					return perMinute(1200)
				})
				return sweepIn(cur, baselineOf(steady()))
			},
			signals: map[string]Status{SignalTraffic: StatusInvestigate},
		},
		{
			name: "traffic ×3 (a campaign): not a sweep finding",
			in: func() SweepInputs {
				cur := steady()
				cur.RequestRate = ago(flat(perMinute(3600)))
				cur.ErrorRate = ago(flat(perMinute(3600) * 0.002))
				return sweepIn(cur, baselineOf(steady()))
			},
			verdict: StatusOK,
			signals: map[string]Status{SignalTraffic: StatusOK},
		},
		{
			name: "no baseline: request-level signals not evaluated, saturation against the previous hour",
			in: func() SweepInputs {
				cur := steady()
				cur.CPU = ago(func(h float64) float64 {
					if h < 1 {
						return 0.60
					}
					return 0.25
				})
				return sweepIn(cur, nil)
			},
			verdict:    StatusDegraded,
			signals:    map[string]Status{SignalLatency: StatusNotEvaluated, SignalTraffic: StatusNotEvaluated, SignalSaturation: StatusDegraded},
			noteSubstr: "no baseline one week or one day earlier",
		},
		{
			name:    "no baseline, nothing wrong: insufficient data, not ok",
			in:      func() SweepInputs { return sweepIn(steady(), nil) },
			verdict: StatusInsufficientData,
		},
		{
			name: "one restart since the previous sweep",
			in: func() SweepInputs {
				in := sweepIn(steady(), baselineOf(steady()))
				in.Instances[0].RestartCount = 1
				return in
			},
			verdict: StatusInvestigate,
			signals: map[string]Status{SignalRestarts: StatusInvestigate},
		},
		{
			name: "first sweep: restarts not evaluated",
			in: func() SweepInputs {
				in := sweepIn(steady(), baselineOf(steady()))
				in.Instances[0].RestartCount = 7
				in.FirstSweep = true
				return in
			},
			verdict: StatusOK,
			signals: map[string]Status{SignalRestarts: StatusNotEvaluated},
		},
		{
			name: "baseline one day earlier is named",
			in: func() SweepInputs {
				in := sweepIn(steady(), baselineOf(steady()))
				in.BaselineAge = 24 * time.Hour
				return in
			},
			verdict:    StatusOK,
			noteSubstr: "one day earlier (nothing one week earlier)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := EvaluateSweep(DefaultConfig(), tc.in())
			if tc.verdict != "" && r.Verdict != tc.verdict {
				t.Errorf("verdict %s, want %s\n%s", r.Verdict, tc.verdict, dump(r))
			}
			for name, want := range tc.signals {
				if got, ok := r.Signal(name); !ok || got.Status != want {
					t.Errorf("signal %s: %+v, want %s\n%s", name, got, want, dump(r))
				}
			}
			if tc.noteSubstr != "" && !strings.Contains(strings.Join(r.Notes, "\n"), tc.noteSubstr) {
				t.Errorf("notes %q lack %q", r.Notes, tc.noteSubstr)
			}
		})
	}
}

// linear goes from a, 6 hours ago, to b now.
func linear(a, b float64) func(float64) float64 {
	return func(h float64) float64 { return b - (b-a)*h/6 }
}

// hourSteps holds each value (MiB) for an hour, oldest first, ending now.
func hourSteps(mib ...float64) func(float64) float64 {
	return func(h float64) float64 {
		i := len(mib) - 1 - int(h)
		if i < 0 {
			i = 0
		}
		return mib[i] * mb
	}
}

func TestMemoryLeak(t *testing.T) {
	tests := []struct {
		name    string
		memory  func(float64) float64
		traffic func(float64) float64 // nil: no traffic metric
		want    Status
		reason  string
	}{
		{"steady rise of 200 MiB (+50%) on stable traffic", linear(400*mb, 600*mb), flat(20), StatusInvestigate, "not explained by load"},
		{"morning ramp: memory follows traffic", linear(400*mb, 600*mb), linear(20, 30), StatusOK, "follows the load"},
		{"memory per request up despite more traffic", linear(400*mb, 800*mb), linear(20, 26), StatusInvestigate, "not explained by load"},
		{"small container: +50% but only 20 MiB", linear(40*mb, 60*mb), flat(20), StatusOK, ""},
		{"large container: +300 MiB but only +7%", linear(4096*mb, 4396*mb), flat(20), StatusOK, ""},
		{"noisy: up and down", func(h float64) float64 {
			if int(h)%2 == 0 {
				return 600 * mb
			}
			return 400 * mb
		}, flat(20), StatusOK, ""},
		{"jumps then drifts down: positive slope but only 2 hourly rises", hourSteps(400, 550, 540, 530, 700, 690, 680), flat(20), StatusOK, ""},
		{"+30% on traffic +5%: memory per request only +24%, but stable traffic rules out load", linear(400*mb, 520*mb), linear(20, 21), StatusInvestigate, "not explained by load"},
		{"worker without a traffic metric", linear(400*mb, 600*mb), nil, StatusNotEvaluated, "without a traffic metric"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := Signals{Memory: ago(tc.memory)}
			if tc.traffic != nil {
				s.RequestRate = ago(tc.traffic)
			}
			got := leakSignal(DefaultConfig(), s, sweepNow)
			if got.Status != tc.want || !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("%+v, want %s (%q)", got, tc.want, tc.reason)
			}
		})
	}

	if got := leakSignal(DefaultConfig(), Signals{}, sweepNow); got.Status != StatusNotEvaluated {
		t.Errorf("no memory data: %+v", got)
	}
	// A leak alone is never worse than investigate (here +35%, under the
	// saturation threshold against the baseline).
	cur := steady()
	cur.Memory = ago(linear(400*mb, 540*mb))
	if r := EvaluateSweep(DefaultConfig(), sweepIn(cur, baselineOf(steady()))); r.Verdict != StatusInvestigate {
		t.Errorf("leak verdict %s\n%s", r.Verdict, dump(r))
	}
}

func TestCollector_CheckSweep(t *testing.T) {
	m := busy(0.120)
	// Nothing older than two days: the one-week baseline is missing.
	m.notBefore = sweepNow.Add(-48 * time.Hour)
	c, orch := newCollector(m, rollout(), sweepNow)
	orch.instances = []provider.Instance{
		{Name: "api-a", Ready: true, RestartCount: 5, Terminations: []provider.Termination{
			{Time: sweepNow.Add(-5 * time.Hour), Reason: "OOMKilled", OOM: true}, // before the previous sweep
			{Time: sweepNow.Add(-time.Hour), Reason: "Error", ExitCode: 1},
		}},
		{Name: "api-b", Ready: true, RestartCount: 1}, // created since
	}
	prev := &PreviousSweep{At: sweepNow.Add(-4 * time.Hour), Restarts: map[string]int{"api-a": 3, "api-gone": 9}}

	r, counts, err := c.CheckSweep(context.Background(), svcAPI, prev)
	if err != nil {
		t.Fatal(err)
	}
	if counts["api-a"] != 5 || counts["api-b"] != 1 || len(counts) != 2 {
		t.Errorf("counts kept for the next sweep: %v", counts)
	}
	// 2 restarts of api-a since the previous sweep plus 1 of the new
	// api-b; the OOM kill predates it.
	if s, _ := r.Signal(SignalRestarts); s.Status != StatusDegraded || !strings.Contains(s.Reason, "3 restart(s), 0 attested OOM") {
		t.Errorf("restarts: %+v", s)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "one day earlier (nothing one week earlier)") {
		t.Errorf("baseline should fall back to one day: %q", r.Notes)
	}
	if orch.revision != "abc123" {
		t.Errorf("instances of revision %q, want the current one", orch.revision)
	}

	if _, _, err := c.CheckSweep(context.Background(), svcAPI, nil); err != nil {
		t.Fatal(err)
	}
}
