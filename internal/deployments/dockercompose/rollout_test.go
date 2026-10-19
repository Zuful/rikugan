package dockercompose

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	orchdc "github.com/Zuful/rikugan/internal/orchestration/dockercompose"
	"github.com/Zuful/rikugan/pkg/provider"
)

var t0 = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

type fakeCompose struct {
	mu       sync.Mutex
	revs     map[string][]orchdc.Revision // by service name
	services []provider.Service
}

func (f *fakeCompose) set(name string, revs ...orchdc.Revision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revs[name] = revs
}

func (f *fakeCompose) Revisions(_ context.Context, svc provider.Service) ([]orchdc.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revs[svc.Name], nil
}

func (f *fakeCompose) ListServices(context.Context, string) ([]provider.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services, nil
}

const (
	oldHash = "1111111111111111aaaa"
	newHash = "2222222222222222bbbb"
)

func ready(hash string, created, readyAt time.Time, n int) orchdc.Revision {
	return orchdc.Revision{Hash: hash, Image: "api:" + hash[:1], Created: created, Containers: n, Ready: n, Live: n, ReadyAt: readyAt}
}

func gone(hash string, created time.Time) orchdc.Revision {
	return orchdc.Revision{Hash: hash, Created: created, Containers: 1}
}

func TestLastRollout(t *testing.T) {
	svc := provider.Service{Name: "api", Namespace: "web"}
	tests := []struct {
		name string
		revs []orchdc.Revision
		now  time.Time
		want provider.Rollout
	}{
		{
			name: "new revision ready, old one gone: complete at the last readiness",
			revs: []orchdc.Revision{ready(newHash, t0, t0.Add(20*time.Second), 2), gone(oldHash, t0.Add(-24*time.Hour))},
			now:  t0.Add(time.Minute),
			want: provider.Rollout{State: provider.RolloutComplete, StartedAt: t0, CompletedAt: t0.Add(20 * time.Second)},
		},
		{
			name: "old revision still running: in progress",
			revs: []orchdc.Revision{ready(newHash, t0, t0.Add(20*time.Second), 1), ready(oldHash, t0.Add(-time.Hour), t0.Add(-time.Hour), 1)},
			now:  t0.Add(time.Minute),
			want: provider.Rollout{State: provider.RolloutInProgress, StartedAt: t0},
		},
		{
			name: "not all new containers ready: in progress",
			revs: []orchdc.Revision{{Hash: newHash, Created: t0, Containers: 2, Ready: 1, Live: 2}},
			now:  t0.Add(5 * time.Minute),
			want: provider.Rollout{State: provider.RolloutInProgress, StartedAt: t0},
		},
		{
			name: "still not ready past the deadline: failed",
			revs: []orchdc.Revision{{Hash: newHash, Created: t0, Containers: 2, Ready: 1, Live: 2}},
			now:  t0.Add(11 * time.Minute),
			want: provider.Rollout{State: provider.RolloutFailed, StartedAt: t0, FailedAt: t0.Add(DefaultRolloutDeadline)},
		},
		{
			name: "a container that restarted after the start: readiness never before the start",
			revs: []orchdc.Revision{ready(newHash, t0, t0.Add(-time.Second), 1)},
			now:  t0.Add(time.Minute),
			want: provider.Rollout{State: provider.RolloutComplete, StartedAt: t0, CompletedAt: t0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCompose{revs: map[string][]orchdc.Revision{"api": tt.revs}}
			got, err := New(f, WithClock(func() time.Time { return tt.now })).LastRollout(context.Background(), svc)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tt.want.State || !got.StartedAt.Equal(tt.want.StartedAt) ||
				!got.CompletedAt.Equal(tt.want.CompletedAt) || !got.FailedAt.Equal(tt.want.FailedAt) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
			if got.Revision != newHash || got.Sequence != "" || got.Label() != "222222222222" || got.Rollback {
				t.Errorf("identity: revision %q sequence %q label %q rollback %v", got.Revision, got.Sequence, got.Label(), got.Rollback)
			}
			if got.State == provider.RolloutFailed && !strings.Contains(got.FailureReason, "compose_rollout_deadline") {
				t.Errorf("failure reason %q", got.FailureReason)
			}
		})
	}

	f := &fakeCompose{revs: map[string][]orchdc.Revision{}}
	if _, err := New(f).LastRollout(context.Background(), svc); err == nil {
		t.Error("a service without containers has no rollout")
	}
}

func TestWatchRollouts(t *testing.T) {
	f := &fakeCompose{
		revs:     map[string][]orchdc.Revision{},
		services: []provider.Service{{Name: "api", Namespace: "web"}},
	}
	f.set("api", ready(oldHash, t0.Add(-time.Hour), t0.Add(-time.Hour), 1))
	now := t0
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := New(f, WithPollInterval(10*time.Millisecond), WithClock(clock)).WatchRollouts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := func(what string) provider.Rollout {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-ctx.Done():
			t.Fatalf("%s: nothing reported", what)
		}
		return provider.Rollout{}
	}

	if r := next("initial"); r.Revision != oldHash || r.State != provider.RolloutComplete {
		t.Fatalf("initial: %+v", r)
	}
	// A new revision starts and never gets ready: failed past the deadline.
	f.set("api", orchdc.Revision{Hash: newHash, Created: t0, Containers: 1, Live: 1})
	mu.Lock()
	now = t0.Add(DefaultRolloutDeadline + time.Minute)
	mu.Unlock()
	if r := next("stalled"); r.Revision != newHash || r.State != provider.RolloutFailed {
		t.Fatalf("stalled: %+v", r)
	}
	// It is fixed in place and gets ready: complete (C2).
	f.set("api", ready(newHash, t0, t0.Add(12*time.Minute), 1))
	if r := next("fixed"); r.Revision != newHash || r.State != provider.RolloutComplete || !r.CompletedAt.Equal(t0.Add(12*time.Minute)) {
		t.Fatalf("fixed: %+v", r)
	}
	cancel()
	for range ch {
	}
}
