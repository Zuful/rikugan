package health

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Series is one aggregated metric over time, typically one sample per
// minute. Rates are per second.
type Series []provider.MetricSample

// Signals are the metric series a check reads. Any of them may be nil
// when the metric does not exist for the service; the matching signal is
// then not evaluated.
type Signals struct {
	RequestRate Series // requests per second
	ErrorRate   Series // 5xx responses per second
	LatencyP95  Series // seconds
	CPU         Series // cores, per replica
	Memory      Series // bytes, per replica
}

// Inputs is everything Evaluate looks at.
type Inputs struct {
	Rollout provider.Rollout
	Now     time.Time
	// Current covers at least the before window and the after window.
	Current Signals
	// Seasonal holds the same series one week earlier, with timestamps
	// shifted forward by 7 days so they line up with Current. Nil when
	// the metrics store has nothing that old.
	Seasonal *Signals
	// Instances are the instances of the rollout's revision.
	Instances []provider.Instance
	// Checkpoint, when set, is the checkpoint to evaluate, instead of
	// the latest one reached at Now. The daemon sets it: it checks each
	// checkpoint in turn, possibly late after a restart.
	Checkpoint time.Duration
	// BeforeEnd, when set, ends the before window instead of the
	// rollout's StartedAt: when this rollout follows another one closely,
	// the before window must end where the last stable period did.
	BeforeEnd time.Time
}

// SelectCheckpoint picks the latest checkpoint a completed rollout has
// reached at now. It returns OutcomeEvaluated with the checkpoint, or
// OutcomeTooEarly / OutcomeStale without one.
func SelectCheckpoint(cfg Config, completedAt, now time.Time) (time.Duration, Outcome) {
	elapsed := now.Sub(completedAt)
	switch {
	case elapsed < Checkpoints[0]:
		return 0, OutcomeTooEarly
	case elapsed > cfg.StaleAfter:
		return 0, OutcomeStale
	}
	cp := Checkpoints[0]
	for _, c := range Checkpoints {
		if elapsed >= c {
			cp = c
		}
	}
	return cp, OutcomeEvaluated
}

// Evaluate runs the rules on one rollout.
func Evaluate(cfg Config, in Inputs) Report {
	r := Report{Rollout: in.Rollout}

	switch in.Rollout.State {
	case provider.RolloutFailed:
		r.Outcome = OutcomeEvaluated
		r.Signals = append(r.Signals, SignalResult{
			Name: SignalRollout, Status: StatusDegraded,
			Reason: "rollout failed: " + in.Rollout.FailureReason,
		})
		r.Signals = append(r.Signals, instanceSignals(cfg, in)...)
		r.Verdict = StatusDegraded
		return r
	case provider.RolloutInProgress:
		r.Outcome = OutcomeInProgress
		r.Notes = append(r.Notes, fmt.Sprintf("rollout in progress for %s", in.Now.Sub(in.Rollout.StartedAt).Round(time.Second)))
		r.Signals = instanceSignals(cfg, in)
		return r
	}

	cp, outcome := SelectCheckpoint(cfg, in.Rollout.CompletedAt, in.Now)
	if in.Checkpoint > 0 && in.Now.Sub(in.Rollout.CompletedAt) >= in.Checkpoint {
		cp, outcome = in.Checkpoint, OutcomeEvaluated
	}
	r.Outcome = outcome
	switch outcome {
	case OutcomeTooEarly:
		r.NextCheck = in.Rollout.CompletedAt.Add(Checkpoints[0])
		r.Signals = instanceSignals(cfg, in)
		return r
	case OutcomeStale:
		r.Notes = append(r.Notes, fmt.Sprintf("last rollout completed %s ago, past the %s limit for a before/after verdict",
			in.Now.Sub(in.Rollout.CompletedAt).Round(time.Minute), cfg.StaleAfter))
		return r
	}
	r.Checkpoint = cp
	if in.Rollout.Rollback {
		r.Notes = append(r.Notes, "rollback to revision "+in.Rollout.Sequence)
	}

	beforeEnd := in.Rollout.StartedAt
	if !in.BeforeEnd.IsZero() && in.BeforeEnd.Before(beforeEnd) {
		beforeEnd = in.BeforeEnd
		r.Notes = append(r.Notes, fmt.Sprintf("before window ends at %s, when the previous rollout started %s before this one: the last stable period",
			beforeEnd.UTC().Format("15:04:05Z"), in.Rollout.StartedAt.Sub(beforeEnd).Round(time.Second)))
	}
	before := window{beforeEnd.Add(-cfg.BeforeWindow), beforeEnd}
	after := window{in.Rollout.CompletedAt, in.Rollout.CompletedAt.Add(cp)}

	volume := volumeGuard(cfg, in.Current.RequestRate, before, after)
	r.Signals = append(r.Signals,
		latencySignal(cfg, in.Current, before, after, volume, cp),
		errorRateSignal(cfg, in.Current, before, after, volume, cp),
	)
	r.Signals = append(r.Signals, instanceSignals(cfg, in)...)
	r.Signals = append(r.Signals, saturationSignal(cfg, in.Current, before, after, cp))
	traffic, note := trafficSignal(cfg, in, before, after)
	r.Signals = append(r.Signals, traffic)
	if note != "" {
		r.Notes = append(r.Notes, note)
	}

	r.Verdict = overall(r.Signals)
	if r.Verdict == StatusOK {
		// An OK needs only one of latency and error rate (design v3.5),
		// so it never hides what it could not look at.
		for _, s := range r.Signals {
			if s.Status == StatusInsufficientData || s.Status == StatusNotEvaluated {
				r.Notes = append(r.Notes, fmt.Sprintf("%s not evaluated: %s", s.Name, s.Reason))
			}
		}
	}
	return r
}

// overall is the worst evaluable status, except that the check is only
// OK when at least one request-level signal could be evaluated: without
// latency nor error rate there is no ground for calling a rollout
// healthy.
func overall(signals []SignalResult) Verdict {
	worst := StatusOK
	requestLevelEvaluated := false
	for _, s := range signals {
		switch s.Status {
		case StatusDegraded:
			worst = StatusDegraded
		case StatusInvestigate:
			if worst != StatusDegraded {
				worst = StatusInvestigate
			}
		}
		if (s.Name == SignalLatency || s.Name == SignalErrorRate) &&
			s.Status != StatusInsufficientData && s.Status != StatusNotEvaluated {
			requestLevelEvaluated = true
		}
	}
	if worst == StatusOK && !requestLevelEvaluated {
		return StatusInsufficientData
	}
	return worst
}

type window struct{ from, to time.Time }

func (w window) duration() time.Duration { return w.to.Sub(w.from) }

func (w window) values(s Series) []float64 {
	var out []float64
	for _, p := range s {
		if !p.Time.Before(w.from) && p.Time.Before(w.to) && !math.IsNaN(p.Value) {
			out = append(out, p.Value)
		}
	}
	return out
}

// count integrates a per-second rate over the window.
func (w window) count(rate Series) (float64, bool) {
	v := w.values(rate)
	if len(v) == 0 {
		return 0, false
	}
	return mean(v) * w.duration().Seconds(), true
}

// median is used for latency and resources: a few warm-up minutes at
// the start of the after window must not drag a whole window's figure.
func (w window) median(s Series) (float64, bool) {
	v := w.values(s)
	if len(v) == 0 {
		return 0, false
	}
	sort.Float64s(v)
	mid := len(v) / 2
	if len(v)%2 == 1 {
		return v[mid], true
	}
	return (v[mid-1] + v[mid]) / 2, true
}

func mean(v []float64) float64 {
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

type volumeCheck struct {
	ok            bool
	before, after float64
	reason        string
}

func volumeGuard(cfg Config, requests Series, before, after window) volumeCheck {
	b, okB := before.count(requests)
	a, okA := after.count(requests)
	if !okB || !okA {
		return volumeCheck{reason: "no request-rate data"}
	}
	vc := volumeCheck{ok: b >= cfg.MinRequests && a >= cfg.MinRequests, before: b, after: a}
	if !vc.ok {
		vc.reason = fmt.Sprintf("%.0f requests before / %.0f after, below the %.0f minimum", b, a, cfg.MinRequests)
	}
	return vc
}

func latencySignal(cfg Config, s Signals, before, after window, vol volumeCheck, cp time.Duration) SignalResult {
	res := SignalResult{Name: SignalLatency}
	if !vol.ok {
		res.Status, res.Reason = StatusInsufficientData, vol.reason
		return res
	}
	b, okB := before.median(s.LatencyP95)
	a, okA := after.median(s.LatencyP95)
	if !okB || !okA {
		res.Status, res.Reason = StatusNotEvaluated, "no latency data"
		return res
	}
	delta := time.Duration((a - b) * float64(time.Second))
	pct := relative(a, b)
	// The figure is the median of the per-minute p95 over each window,
	// not the p95 of the window's requests, and is named as such.
	res.Reason = fmt.Sprintf("median p95 over the window %s → %s (%+.0f%%, %+s)", seconds(b), seconds(a), pct*100, delta.Round(time.Millisecond))
	switch {
	case pct >= cfg.LatencyDegradedPct && delta >= cfg.LatencyDegradedAbs:
		res.Status = StatusDegraded
		if cp < cfg.LatencyDegradedFromCheckpoint {
			res.Status = StatusInvestigate
			res.Reason += fmt.Sprintf("; capped at investigate before +%s (warm-up)", cfg.LatencyDegradedFromCheckpoint)
		}
	case pct >= cfg.LatencyInvestigatePct && delta >= cfg.LatencyInvestigateAbs:
		res.Status = StatusInvestigate
	default:
		res.Status = StatusOK
	}
	return res
}

func errorRateSignal(cfg Config, s Signals, before, after window, vol volumeCheck, cp time.Duration) SignalResult {
	res := SignalResult{Name: SignalErrorRate}
	if !vol.ok {
		return bluntErrorRule(cfg, s, after, vol, cp)
	}
	eb, okB := before.count(s.ErrorRate)
	ea, okA := after.count(s.ErrorRate)
	if !okB || !okA {
		res.Status, res.Reason = StatusNotEvaluated, "no error-rate data"
		return res
	}
	rb, ra := eb/vol.before, ea/vol.after
	res.Reason = fmt.Sprintf("5xx %.2f%% → %.2f%% (%.0f errors after)", rb*100, ra*100, ea)
	grew := ra >= cfg.ErrorRatio*rb && ra > 0
	switch {
	case grew && ra > cfg.ErrorDegradedRate && ea >= cfg.ErrorDegradedMinErr:
		res.Status = StatusDegraded
	case grew && ra > cfg.ErrorInvestigateRate && ea >= cfg.ErrorInvestigateMinErr:
		res.Status = StatusInvestigate
	default:
		res.Status = StatusOK
	}
	return res
}

// bluntErrorRule handles an error rate below the volume guard. With
// fallback A, the checkpoints before the last one still catch a blunt
// regression; anything subtler waits for the last checkpoint, whose
// window holds the most requests. The before window plays no part: with
// so few requests, a ratio to it would mean little.
func bluntErrorRule(cfg Config, s Signals, after window, vol volumeCheck, cp time.Duration) SignalResult {
	res := SignalResult{Name: SignalErrorRate, Status: StatusInsufficientData, Reason: vol.reason}
	if !cfg.BluntRule || cp >= Checkpoints[len(Checkpoints)-1] || vol.after <= 0 {
		return res
	}
	ea, ok := after.count(s.ErrorRate)
	if !ok {
		return res
	}
	ra := ea / vol.after
	if ra > cfg.BluntErrorRate && ea >= cfg.BluntErrorMinErr {
		res.Status = StatusDegraded
		res.Reason = fmt.Sprintf("5xx %.1f%% (%.0f errors in %.0f requests) after, a blunt regression despite the low volume", ra*100, ea, vol.after)
		return res
	}
	res.Reason += fmt.Sprintf("; blunt rule (5xx above %.0f%% with at least %.0f errors) not triggered", cfg.BluntErrorRate*100, cfg.BluntErrorMinErr)
	return res
}

// instanceSignals reads the new revision's instances: restarts and
// availability. They need no traffic, so they are never subject to the
// volume guard, and they are reported even when there is no verdict
// (rollout in progress, too early).
func instanceSignals(cfg Config, in Inputs) []SignalResult {
	return []SignalResult{restartSignal(in.Instances), availabilitySignal(cfg, in)}
}

func restartSignal(instances []provider.Instance) SignalResult {
	res := SignalResult{Name: SignalRestarts, Status: StatusOK}
	restarts, oom, looping := 0, 0, 0
	for _, inst := range instances {
		restarts += inst.RestartCount
		if inst.CrashLooping {
			looping++
		}
		for _, t := range inst.Terminations {
			if t.OOM {
				oom++
			}
		}
	}
	res.Reason = fmt.Sprintf("%d restart(s), %d attested OOM kill(s), %d crash-looping instance(s)", restarts, oom, looping)
	switch {
	case oom > 0 || looping > 0 || restarts >= 2:
		res.Status = StatusDegraded
	case restarts == 1:
		res.Status = StatusInvestigate
	}
	return res
}

// availabilitySignal flags instances that have been unready for longer
// than their grace period. Instances created by the rollout were ready
// when it completed, so they get the short grace; instances added later
// (scale-out, replacement) are still starting and get the long one.
func availabilitySignal(cfg Config, in Inputs) SignalResult {
	res := SignalResult{Name: SignalAvailability, Status: StatusOK, Reason: fmt.Sprintf("%d instance(s), all ready or within their grace period", len(in.Instances))}
	for _, inst := range in.Instances {
		if inst.Ready {
			continue
		}
		grace := cfg.UnavailableGrace
		if !in.Rollout.CompletedAt.IsZero() && !inst.Created.After(in.Rollout.CompletedAt) {
			grace = cfg.RolloutUnavailableGrace
		}
		if unready := in.Now.Sub(inst.NotReadySince); unready > grace {
			res.Status = StatusDegraded
			res.Reason = fmt.Sprintf("instance %s not ready for %s (grace %s)", inst.Name, unready.Round(time.Second), grace)
			return res
		}
	}
	return res
}

func saturationSignal(cfg Config, s Signals, before, after window, cp time.Duration) SignalResult {
	res := SignalResult{Name: SignalSaturation}
	if cp < cfg.SaturationFromCheckpoint {
		res.Status, res.Reason = StatusNotEvaluated, fmt.Sprintf("not evaluated before +%s (warm-up)", cfg.SaturationFromCheckpoint)
		return res
	}
	res.Status = StatusNotEvaluated
	res.Reason = "no CPU or memory data"
	for _, m := range []struct {
		name string
		s    Series
	}{{"CPU", s.CPU}, {"memory", s.Memory}} {
		b, okB := before.median(m.s)
		a, okA := after.median(m.s)
		if !okB || !okA || b <= 0 {
			continue
		}
		pct := relative(a, b)
		st := StatusOK
		switch {
		case pct >= cfg.SaturationDegradedPct:
			st = StatusDegraded
		case pct >= cfg.SaturationInvestigatePct:
			st = StatusInvestigate
		}
		if res.Status == StatusNotEvaluated || worse(st, res.Status) {
			res.Status = st
			res.Reason = fmt.Sprintf("%s per replica %+.0f%%", m.name, pct*100)
		}
	}
	return res
}

// trafficSignal: traffic is mostly driven by things other than the
// deploy, so a deviation only counts when it shows against both the
// before window and the same window a week earlier. A collapse towards
// zero counts on the before window alone. Traffic never makes a check
// worse than "investigate".
func trafficSignal(cfg Config, in Inputs, before, after window) (SignalResult, string) {
	res := SignalResult{Name: SignalTraffic, Status: StatusOK}
	rb := before.values(in.Current.RequestRate)
	ra := after.values(in.Current.RequestRate)
	if len(rb) == 0 || len(ra) == 0 || mean(rb) == 0 {
		res.Status, res.Reason = StatusNotEvaluated, "no request-rate data"
		return res, ""
	}
	vsBefore := mean(ra) / mean(rb)
	res.Reason = fmt.Sprintf("%.1f → %.1f req/s (×%.2f vs before)", mean(rb), mean(ra), vsBefore)

	if vsBefore <= cfg.TrafficZeroRatio {
		res.Status = StatusInvestigate
		res.Reason += ": traffic collapsed"
		return res, ""
	}
	outBefore := vsBefore <= cfg.TrafficDropRatio || vsBefore >= cfg.TrafficRiseRatio

	var seasonal []float64
	if in.Seasonal != nil {
		seasonal = after.values(in.Seasonal.RequestRate)
	}
	if len(seasonal) == 0 || mean(seasonal) == 0 {
		if outBefore {
			res.Status = StatusInvestigate
		}
		return res, "seasonality guard disabled: no data one week earlier"
	}
	vsWeek := mean(ra) / mean(seasonal)
	res.Reason += fmt.Sprintf(", ×%.2f vs one week earlier", vsWeek)
	outWeek := vsWeek <= cfg.TrafficDropRatio || vsWeek >= cfg.TrafficRiseRatio
	if outBefore && outWeek {
		res.Status = StatusInvestigate
	}
	return res, ""
}

func relative(after, before float64) float64 {
	if before == 0 {
		if after == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return after/before - 1
}

func worse(a, b Status) bool { return rank(a) > rank(b) }

func rank(s Status) int {
	switch s {
	case StatusDegraded:
		return 3
	case StatusInvestigate:
		return 2
	case StatusOK:
		return 1
	}
	return 0
}

func seconds(v float64) time.Duration {
	return time.Duration(v * float64(time.Second)).Round(time.Millisecond)
}
