// Package tracker turns successive observations of a service's current
// rollout into the events DeploymentSource.WatchRollouts reports:
// completions, failures and superseded rollouts, each once. Every
// DeploymentSource backend reconstructs the current rollout its own way
// and shares this bookkeeping.
package tracker

import (
	"strconv"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Tracker remembers, per service, the last rollout seen and the states
// already reported, so each (sequence, state) is emitted once.
type Tracker struct {
	last map[string]tracked
}

type tracked struct {
	rollout  provider.Rollout
	reported map[provider.RolloutState]bool
}

// New returns an empty Tracker.
func New() *Tracker { return &Tracker{last: map[string]tracked{}} }

// Forget drops a service, typically one that was deleted.
func (t *Tracker) Forget(key string) { delete(t.last, key) }

// Keys lists the services tracked.
func (t *Tracker) Keys() []string {
	keys := make([]string, 0, len(t.last))
	for k := range t.last {
		keys = append(keys, k)
	}
	return keys
}

// Observe takes the service's current rollout and returns what to
// report, oldest first:
//   - the previous rollout, as superseded, if a newer one started before
//     it ended;
//   - a superseded rollout for every revision number skipped between two
//     observations: two rollouts can start before the first event is
//     processed, and the reconstruction only ever sees the last one;
//   - the current rollout, if it has ended in a state not reported yet.
func (t *Tracker) Observe(key string, r provider.Rollout) []provider.Rollout {
	var out []provider.Rollout
	prev, seen := t.last[key]
	if seen && prev.rollout.Label() != r.Label() {
		if !prev.ended() {
			s := prev.rollout
			s.State = provider.RolloutSuperseded
			out = append(out, s)
		}
		out = append(out, skipped(prev.rollout, r)...)
		prev = tracked{}
	}
	if prev.reported == nil {
		prev.reported = map[provider.RolloutState]bool{}
	}
	prev.rollout = r
	if (r.State == provider.RolloutComplete || r.State == provider.RolloutFailed) && !prev.reported[r.State] {
		prev.reported[r.State] = true
		out = append(out, r)
	}
	t.last[key] = prev
	return out
}

// ended reports whether the rollout completed or failed: a newer one
// does not supersede it.
func (tr tracked) ended() bool {
	return tr.reported[provider.RolloutComplete] || tr.reported[provider.RolloutFailed] ||
		tr.rollout.State == provider.RolloutComplete || tr.rollout.State == provider.RolloutFailed
}

// skipped returns a superseded rollout for each revision number strictly
// between prev and cur. Only the service and the sequence are known: the
// reconstruction never saw them.
func skipped(prev, cur provider.Rollout) []provider.Rollout {
	from, err1 := strconv.Atoi(prev.Sequence)
	to, err2 := strconv.Atoi(cur.Sequence)
	if err1 != nil || err2 != nil || to-from > 100 { // a gap that large is not a burst of rollouts
		return nil
	}
	var out []provider.Rollout
	for n := from + 1; n < to; n++ {
		out = append(out, provider.Rollout{Service: cur.Service, Sequence: strconv.Itoa(n), State: provider.RolloutSuperseded})
	}
	return out
}
