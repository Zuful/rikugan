// Package health is the proactive mode's rules engine: given a rollout,
// the metric series around it and the instances of its new revision, it
// decides whether the rollout broke something.
//
// Evaluate is a pure function — no I/O, no clock — so every rule is
// covered by fixtures that replay realistic deploy profiles. Fetching the
// inputs through the concept interfaces is the Collector's job.
package health

import "time"

// Checkpoints are the fixed delays after a rollout's completion at which
// it is evaluated. The "after" window of a check always ends at its
// checkpoint; it never stretches to "now".
var Checkpoints = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}

// Config holds every threshold of the rules. DefaultConfig documents the
// values; operators override them per service.
type Config struct {
	// BeforeWindow is the comparison window that ends when the rollout
	// started.
	BeforeWindow time.Duration
	// StaleAfter is how long after completion a rollout is still worth
	// a before/after verdict. Past it, instance signals (availability
	// now, restarts since the rollout) mix the rollout with everything
	// that happened since.
	StaleAfter time.Duration

	// MinRequests is the volume guard: below it, in the before or the
	// after window, latency and error rate are not evaluated.
	MinRequests float64

	LatencyInvestigatePct float64
	LatencyInvestigateAbs time.Duration
	LatencyDegradedPct    float64
	LatencyDegradedAbs    time.Duration
	// LatencyDegradedFromCheckpoint caps latency at "investigate" before
	// this checkpoint: a slow warm-up puts the median of a 5-minute window
	// on its spike (design v3.5).
	LatencyDegradedFromCheckpoint time.Duration

	ErrorRatio             float64 // after rate must be at least this multiple of before
	ErrorInvestigateRate   float64
	ErrorInvestigateMinErr float64
	ErrorDegradedRate      float64
	ErrorDegradedMinErr    float64

	// BluntRule is fallback A of the volume guard (design v3.5): at the
	// checkpoints before the last one, a window below MinRequests still
	// reports a blunt error-rate regression (above BluntErrorRate with at
	// least BluntErrorMinErr errors) as degraded. Off, such windows are
	// only "insufficient data", the rule before v3.5.
	BluntRule        bool
	BluntErrorRate   float64
	BluntErrorMinErr float64

	SaturationInvestigatePct float64
	SaturationDegradedPct    float64
	SaturationFromCheckpoint time.Duration // warm-up: not evaluated before this

	TrafficDropRatio float64 // after/before at or below this is a drop
	TrafficRiseRatio float64 // after/before at or above this is a rise
	TrafficZeroRatio float64 // after/before at or below this is a collapse, on one baseline alone

	UnavailableGrace        time.Duration // instances added after the rollout
	RolloutUnavailableGrace time.Duration // instances created by the rollout

	// Memory-leak detection of the periodic sweep (design §6): memory
	// per replica up over the last LeakSpan by at least both thresholds,
	// and not explained by traffic.
	LeakSpan             time.Duration
	LeakMinIncreasePct   float64
	LeakMinIncreaseBytes float64
	// LeakMaxTrafficGrowth is the traffic growth over LeakSpan under
	// which a memory rise cannot be load.
	LeakMaxTrafficGrowth float64
}

// DefaultConfig returns the thresholds of the design (issue #4, v3.5).
func DefaultConfig() Config {
	return Config{
		BeforeWindow: 30 * time.Minute,
		StaleAfter:   2 * time.Hour,

		MinRequests: 200,

		LatencyInvestigatePct: 0.30,
		LatencyInvestigateAbs: 50 * time.Millisecond,
		LatencyDegradedPct:    1.00,
		LatencyDegradedAbs:    200 * time.Millisecond,

		LatencyDegradedFromCheckpoint: 15 * time.Minute,

		ErrorRatio:             2,
		ErrorInvestigateRate:   0.01,
		ErrorInvestigateMinErr: 10,
		ErrorDegradedRate:      0.05,
		ErrorDegradedMinErr:    25,

		BluntRule:        true,
		BluntErrorRate:   0.20,
		BluntErrorMinErr: 10,

		SaturationInvestigatePct: 0.40,
		SaturationDegradedPct:    1.00,
		SaturationFromCheckpoint: 15 * time.Minute,

		TrafficDropRatio: 0.50,
		TrafficRiseRatio: 3.00,
		TrafficZeroRatio: 0.05,

		UnavailableGrace:        2 * time.Minute,
		RolloutUnavailableGrace: 60 * time.Second,

		LeakSpan:             6 * time.Hour,
		LeakMinIncreasePct:   0.25,
		LeakMinIncreaseBytes: 100 << 20,
		LeakMaxTrafficGrowth: 0.10,
	}
}
