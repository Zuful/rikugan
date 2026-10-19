package tracker

import (
	"strings"
	"testing"

	"github.com/Zuful/rikugan/pkg/provider"
)

func svc() provider.Service {
	return provider.Service{Name: "api", Namespace: "prod", Kind: "deployment"}
}

func rollout(seq string, state provider.RolloutState) provider.Rollout {
	return provider.Rollout{Service: svc(), Revision: "h" + seq, Sequence: seq, State: state}
}

func events(rs []provider.Rollout) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Label()+":"+string(r.State))
	}
	return strings.Join(out, " ")
}

func TestTrackerObserve(t *testing.T) {
	const (
		prog = provider.RolloutInProgress
		done = provider.RolloutComplete
		fail = provider.RolloutFailed
	)
	tests := []struct {
		name string
		seen []provider.Rollout
		want []string // per observation
	}{
		{
			name: "in progress then complete: reported once",
			seen: []provider.Rollout{rollout("2", prog), rollout("2", done), rollout("2", done)},
			want: []string{"", "2:complete", ""},
		},
		{
			name: "already complete at start: reported (consumer deduplicates)",
			seen: []provider.Rollout{rollout("2", done)},
			want: []string{"2:complete"},
		},
		{
			name: "failed then completed: both reported",
			seen: []provider.Rollout{rollout("2", prog), rollout("2", fail), rollout("2", fail), rollout("2", done)},
			want: []string{"", "2:failed", "", "2:complete"},
		},
		{
			name: "a newer rollout starts before the current one ends: superseded",
			seen: []provider.Rollout{rollout("2", prog), rollout("3", prog), rollout("3", done)},
			want: []string{"", "2:superseded", "3:complete"},
		},
		{
			name: "a completed rollout is not superseded by the next one",
			seen: []provider.Rollout{rollout("2", done), rollout("3", prog)},
			want: []string{"2:complete", ""},
		},
		{
			name: "a failed rollout is not superseded by the fix-forward",
			seen: []provider.Rollout{rollout("2", fail), rollout("3", done)},
			want: []string{"2:failed", "3:complete"},
		},
		{
			name: "two rollouts between observations: the skipped revision is superseded",
			seen: []provider.Rollout{rollout("2", done), rollout("4", done)},
			want: []string{"2:complete", "3:superseded 4:complete"},
		},
		{
			name: "in progress, then two revisions later",
			seen: []provider.Rollout{rollout("2", prog), rollout("4", prog)},
			want: []string{"", "2:superseded 3:superseded"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := New()
			for i, r := range tt.seen {
				if got := events(tr.Observe("prod/api", r)); got != tt.want[i] {
					t.Errorf("observation %d (%s:%s): got %q, want %q", i, r.Sequence, r.State, got, tt.want[i])
				}
			}
		})
	}
}

func TestTrackerSkippedCarriesOnlyWhatIsKnown(t *testing.T) {
	tr := New()
	tr.Observe("prod/api", rollout("2", provider.RolloutComplete))
	got := tr.Observe("prod/api", rollout("4", provider.RolloutInProgress))
	if len(got) != 1 || got[0].Sequence != "3" || got[0].Revision != "" || got[0].Service.Name != "api" {
		t.Errorf("skipped rollout: %+v", got)
	}
}

// Compose numbers no revision: rollouts are told apart by revision, and
// no revision is ever reported skipped.
func TestTrackerWithoutSequence(t *testing.T) {
	tr := New()
	compose := func(hash string, state provider.RolloutState) provider.Rollout {
		return provider.Rollout{Service: svc(), Revision: hash, State: state}
	}
	var got []string
	for _, r := range []provider.Rollout{
		compose("aaaa", provider.RolloutInProgress),
		compose("bbbb", provider.RolloutInProgress),
		compose("bbbb", provider.RolloutComplete),
	} {
		got = append(got, events(tr.Observe("prod/api", r)))
	}
	if want := []string{"", "aaaa:superseded", "bbbb:complete"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events %q, want %q", got, want)
	}
}
