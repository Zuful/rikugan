package health

import (
	"fmt"
	"math"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// The periodic sweep (design §6) covers what rollouts do not: slow
// degradation, a dependency going down, a memory leak. There is no
// rollout to anchor on, so the last hour is compared with the same hour
// one week earlier (one day earlier when the week is missing).

// SignalMemoryLeak is the sweep's memory-leak signal.
const SignalMemoryLeak = "memory_leak"

// SweepWindow is the window a sweep evaluates: the last hour.
const SweepWindow = time.Hour

// SweepInputs is everything EvaluateSweep looks at.
type SweepInputs struct {
	Now time.Time
	// Current covers at least the last LeakSpan (plus a few minutes) up
	// to Now.
	Current Signals
	// Baseline holds the last hour one week (or one day) earlier, with
	// timestamps shifted forward so they line up with Current's last
	// hour. Nil when the metrics store has neither.
	Baseline *Signals
	// BaselineAge is how far back Baseline was taken: 7 days or 1 day.
	BaselineAge time.Duration
	// Instances are the service's current instances, with RestartCount
	// and Terminations restricted to what happened since the previous
	// sweep.
	Instances []provider.Instance
	// FirstSweep is set when there is no previous sweep to count
	// restarts from: restarts are then not evaluated.
	FirstSweep bool
}

// EvaluateSweep runs the sweep's rules on one service.
//
// Latency, error rate and saturation use the rollout rules, with the
// baseline hour as the before window and the last hour as the after
// window. Traffic only flags a collapse: without a change, any other
// deviation is exogenous by definition. Restarts count only those since
// the previous sweep, and availability uses unavailable_grace for every
// instance. Without any baseline, saturation is compared with the
// previous hour and request-level signals are not evaluated.
func EvaluateSweep(cfg Config, in SweepInputs) Report {
	r := Report{Outcome: OutcomeEvaluated}

	after := window{in.Now.Add(-SweepWindow), in.Now}
	before := window{in.Now.Add(-2 * SweepWindow), in.Now.Add(-SweepWindow)}

	// The existing rules compare two windows of the same series, so the
	// baseline hour is moved back one hour, just before the last one.
	merged := in.Current
	if in.Baseline != nil {
		merged = Signals{
			RequestRate: splice(in.Baseline.RequestRate, in.Current.RequestRate, after.from),
			ErrorRate:   splice(in.Baseline.ErrorRate, in.Current.ErrorRate, after.from),
			LatencyP95:  splice(in.Baseline.LatencyP95, in.Current.LatencyP95, after.from),
			CPU:         splice(in.Baseline.CPU, in.Current.CPU, after.from),
			Memory:      splice(in.Baseline.Memory, in.Current.Memory, after.from),
		}
		r.Notes = append(r.Notes, "baseline: the same hour "+ageLabel(in.BaselineAge))
	} else {
		r.Notes = append(r.Notes, "no baseline one week or one day earlier: latency, error rate and traffic not evaluated; saturation compared with the previous hour")
	}

	if in.Baseline != nil {
		volume := volumeGuard(cfg, merged.RequestRate, before, after)
		r.Signals = append(r.Signals,
			latencySignal(cfg, merged, before, after, volume, SweepWindow),
			errorRateSignal(cfg, merged, before, after, volume, SweepWindow),
		)
	} else {
		reason := "no baseline"
		r.Signals = append(r.Signals,
			SignalResult{Name: SignalLatency, Status: StatusNotEvaluated, Reason: reason},
			SignalResult{Name: SignalErrorRate, Status: StatusNotEvaluated, Reason: reason},
		)
	}

	restarts := restartSignal(in.Instances)
	if in.FirstSweep {
		restarts = SignalResult{Name: SignalRestarts, Status: StatusNotEvaluated,
			Reason: "first sweep: restart counts recorded, restarts are counted from the next sweep"}
	}
	r.Signals = append(r.Signals, restarts, availabilitySignal(cfg, Inputs{Now: in.Now, Instances: in.Instances}))
	r.Signals = append(r.Signals, saturationSignal(cfg, merged, before, after, SweepWindow))
	r.Signals = append(r.Signals, sweepTrafficSignal(cfg, merged, before, after, in.Baseline != nil))
	r.Signals = append(r.Signals, leakSignal(cfg, in.Current, in.Now))

	r.Verdict = overall(r.Signals)
	return r
}

func ageLabel(age time.Duration) string {
	switch age {
	case 7 * 24 * time.Hour:
		return "one week earlier"
	case 24 * time.Hour:
		return "one day earlier (nothing one week earlier)"
	}
	return age.String() + " earlier"
}

// splice returns the baseline hour moved back one window, so it falls
// just before start, followed by the current points from start on.
func splice(baseline, current Series, start time.Time) Series {
	var out Series
	end := start.Add(SweepWindow)
	for _, p := range baseline {
		// [start, end) only: a baseline point at end would land on the
		// first instant of the current window once moved back.
		if !p.Time.Before(start) && p.Time.Before(end) {
			out = append(out, provider.MetricSample{Time: p.Time.Add(-SweepWindow), Value: p.Value})
		}
	}
	for _, p := range current {
		if !p.Time.Before(start) {
			out = append(out, p)
		}
	}
	return out
}

// sweepTrafficSignal only flags a collapse towards zero against the
// baseline: outside a change, a traffic shift is a campaign, a season,
// a holiday. Never worse than investigate.
func sweepTrafficSignal(cfg Config, s Signals, before, after window, hasBaseline bool) SignalResult {
	res := SignalResult{Name: SignalTraffic, Status: StatusNotEvaluated, Reason: "no baseline"}
	if !hasBaseline {
		return res
	}
	rb, ra := before.values(s.RequestRate), after.values(s.RequestRate)
	if len(rb) == 0 || len(ra) == 0 || mean(rb) == 0 {
		res.Reason = "no request-rate data"
		return res
	}
	ratio := mean(ra) / mean(rb)
	res.Status = StatusOK
	res.Reason = fmt.Sprintf("%.1f → %.1f req/s (×%.2f vs baseline)", mean(rb), mean(ra), ratio)
	if ratio <= cfg.TrafficZeroRatio {
		res.Status = StatusInvestigate
		res.Reason += ": traffic collapsed"
	}
	return res
}

// leakSignal applies the sweep's three conditions for a memory leak,
// per replica, over the last LeakSpan (design §6):
//
//  1. trend: memory up on hourly points, with a positive regression
//     slope and a rise in all but at most one interval;
//  2. size: the total rise reaches both LeakMinIncreasePct and
//     LeakMinIncreaseBytes, relative for small containers, absolute for
//     large ones;
//  3. not load: traffic grew at most LeakMaxTrafficGrowth, or memory per
//     request grew by at least LeakMinIncreasePct.
//
// Without a traffic metric the third condition cannot be checked, and
// the leak is reported not evaluable rather than guessed. A leak is at
// most "investigate".
func leakSignal(cfg Config, s Signals, now time.Time) SignalResult {
	res := SignalResult{Name: SignalMemoryLeak, Status: StatusNotEvaluated}
	hours := int(cfg.LeakSpan / time.Hour)
	if hours < 2 {
		res.Reason = "leak span shorter than two hours"
		return res
	}
	mem, okM := hourly(s.Memory, now, hours)
	if !okM {
		res.Reason = fmt.Sprintf("not enough memory data over the last %s", cfg.LeakSpan)
		return res
	}
	first, last := mem[0], mem[len(mem)-1]
	rises := 0
	for i := 1; i < len(mem); i++ {
		if mem[i] > mem[i-1] {
			rises++
		}
	}
	growth := relative(last, first)
	trend := slope(mem) > 0 && rises >= len(mem)-2
	size := growth >= cfg.LeakMinIncreasePct && last-first >= cfg.LeakMinIncreaseBytes
	res.Status = StatusOK
	res.Reason = fmt.Sprintf("memory per replica %s → %s over %s (%+.0f%%, %d/%d hourly rises)",
		mib(first), mib(last), cfg.LeakSpan, growth*100, rises, len(mem)-1)
	if !trend || !size {
		return res
	}

	req, okR := hourly(s.RequestRate, now, hours)
	if !okR || req[0] <= 0 {
		res.Status = StatusNotEvaluated
		res.Reason += "; possible leak, but without a traffic metric load cannot be ruled out"
		return res
	}
	traffic := relative(req[len(req)-1], req[0])
	perRequest := relative(last/req[len(req)-1], first/req[0])
	if traffic <= cfg.LeakMaxTrafficGrowth || perRequest >= cfg.LeakMinIncreasePct {
		res.Status = StatusInvestigate
		res.Reason += fmt.Sprintf("; traffic %+.0f%%, memory per request %+.0f%%: not explained by load", traffic*100, perRequest*100)
		return res
	}
	res.Reason += fmt.Sprintf("; traffic %+.0f%%, memory per request %+.0f%%: follows the load", traffic*100, perRequest*100)
	return res
}

// hourly returns hours+1 points, one per hour from now-hours to now,
// each the mean of the ten minutes before it. It reports false when any
// point has no data.
func hourly(s Series, now time.Time, hours int) ([]float64, bool) {
	out := make([]float64, 0, hours+1)
	for h := hours; h >= 0; h-- {
		at := now.Add(-time.Duration(h) * time.Hour)
		v := window{at.Add(-10 * time.Minute), at.Add(time.Nanosecond)}.values(s)
		if len(v) == 0 {
			return nil, false
		}
		out = append(out, mean(v))
	}
	return out, true
}

// slope is the least-squares slope of v against its index.
func slope(v []float64) float64 {
	n := float64(len(v))
	var sx, sy, sxy, sxx float64
	for i, y := range v {
		x := float64(i)
		sx += x
		sy += y
		sxy += x * y
		sxx += x * x
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}

func mib(v float64) string {
	return fmt.Sprintf("%.0f MiB", math.Round(v/(1<<20)))
}
