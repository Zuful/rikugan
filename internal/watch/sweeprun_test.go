package watch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/watch/sweep"
	"github.com/Zuful/rikugan/pkg/provider"
)

type fakeServices struct {
	provider.OrchestrationProvider
	byNS map[string][]provider.Service
}

func (f *fakeServices) ListServices(_ context.Context, ns string) ([]provider.Service, error) {
	if s, ok := f.byNS[ns]; ok {
		return s, nil
	}
	return nil, errors.New("namespace not found")
}

// sweepChecker returns, per service, the next verdict of its queue.
type sweepChecker struct {
	queue map[string][]health.Status
	prevs map[string][]*health.PreviousSweep
	fail  map[string]bool
	// err is returned along with the report, as with a failing
	// metrics backend.
	err error
}

func (c *sweepChecker) CheckSweep(_ context.Context, svc provider.Service, prev *health.PreviousSweep) (health.Report, map[string]int, error) {
	key := serviceKey(svc)
	if c.prevs == nil {
		c.prevs = map[string][]*health.PreviousSweep{}
	}
	c.prevs[key] = append(c.prevs[key], prev)
	if c.fail[key] {
		return health.Report{}, nil, errors.New("prometheus unreachable")
	}
	v := health.StatusOK
	if q := c.queue[key]; len(q) > 0 {
		v, c.queue[key] = q[0], q[1:]
	}
	return health.Report{Rollout: provider.Rollout{Sequence: "3"}, Outcome: health.OutcomeEvaluated, Verdict: v,
			Signals: []health.SignalResult{{Name: "memory_leak", Status: v, Reason: "memory per replica 400 MiB → 600 MiB"}}},
		map[string]int{key + "-pod": len(c.prevs[key])}, c.err
}

func svcIn(ns, name string) provider.Service {
	return provider.Service{Namespace: ns, Name: name, Kind: "deployment"}
}

func sweepHarness(t *testing.T, queue map[string][]health.Status) (*harness, *sweepChecker, *fakeChannel) {
	h := newHarness(t)
	h.source.bySvc = map[string]provider.Rollout{}
	chk := &sweepChecker{queue: queue}
	tg := &fakeChannel{name: "telegram"}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.d.Sweep = &SweepConfig{
		Namespaces: []string{"shop", "missing"},
		Exclude:    []string{"batch", "shop/legacy"},
		Services: &fakeServices{byNS: map[string][]provider.Service{
			"shop": {svcIn("shop", "api"), svcIn("shop", "web"), svcIn("shop", "batch"), svcIn("shop", "legacy"), svcIn("shop", "fresh")},
		}},
		Checker: chk,
	}
	// shop/fresh completed a rollout 30 minutes ago: rollout checks cover it.
	h.source.bySvc["shop/fresh"] = provider.Rollout{State: provider.RolloutComplete, CompletedAt: t0.Add(-30 * time.Minute)}
	// shop/rolling is in the middle of a rollout: covered too.
	h.d.Sweep.Services.(*fakeServices).byNS["shop"] = append(h.d.Sweep.Services.(*fakeServices).byNS["shop"], svcIn("shop", "rolling"))
	h.source.bySvc["shop/rolling"] = provider.Rollout{State: provider.RolloutInProgress, StartedAt: t0.Add(-time.Minute)}
	// shop/api and shop/web last rolled out days ago.
	for _, k := range []string{"shop/api", "shop/web"} {
		h.source.bySvc[k] = provider.Rollout{State: provider.RolloutComplete, CompletedAt: t0.Add(-72 * time.Hour), Sequence: "3"}
	}
	return h, chk, tg
}

func (h *harness) restartWithSweep(sw *SweepConfig, n *Notifier) {
	h.restart()
	h.d.Sweep, h.d.Notifier = sw, n
	if err := h.d.Replay(); err != nil {
		h.t.Fatal(err)
	}
}

func TestSweepScopeAndSchedule(t *testing.T) {
	h, chk, _ := sweepHarness(t, nil)
	next := h.at(0).due()
	if _, swept := chk.prevs["shop/api"]; !swept || len(chk.prevs) != 2 {
		t.Fatalf("swept %v, want shop/api and shop/web only (excluded, covered and unlisted left out)", keysOf(chk.prevs))
	}
	if !next.Equal(t0.Add(DefaultSweepInterval)) {
		t.Errorf("next due %s, want one interval later", next)
	}
	if chk.prevs["shop/api"][0] != nil {
		t.Error("the first sweep of a service has no previous sweep")
	}

	h.at(time.Hour).due() // not due yet
	if len(chk.prevs["shop/api"]) != 1 {
		t.Errorf("swept again after an hour")
	}
	h.at(DefaultSweepInterval).due()
	prev := chk.prevs["shop/api"][1]
	if prev == nil || !prev.At.Equal(t0) || prev.Restarts["shop/api-pod"] != 1 {
		t.Errorf("second sweep's previous sweep: %+v", prev)
	}
	// By then shop/fresh's rollout is 4.5 h old: no longer covered.
	if _, swept := chk.prevs["shop/fresh"]; !swept {
		t.Error("shop/fresh not swept once its rollout is past stale_after")
	}
	if n := h.count(KindSweep + " "); n != 5 {
		t.Errorf("%d sweep records, want 5", n)
	}
}

func TestSweepNotificationsFollowTheStateMachine(t *testing.T) {
	inv, deg, ok := health.StatusInvestigate, health.StatusDegraded, health.StatusOK
	h, _, tg := sweepHarness(t, map[string][]health.Status{
		"shop/api": {inv, inv, inv, inv, deg, ok, ok},
	})
	for i := range 7 {
		h.at(time.Duration(i) * DefaultSweepInterval).due()
	}
	want := strings.Join([]string{
		"[warning] shop/api: investigate for 3 sweeps in a row",
		"[critical] shop/api: degraded (periodic sweep)",
		"[info] shop/api: resolved, two sweeps ok in a row",
	}, "\n")
	if got := tg.titles(); got != want {
		t.Errorf("messages:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(tg.msgs[0].Body, "memory_leak: investigate") {
		t.Errorf("the persistent warning does not say why:\n%s", tg.msgs[0].Body)
	}
}

func TestSweepStateSurvivesARestart(t *testing.T) {
	inv := health.StatusInvestigate
	h, chk, tg := sweepHarness(t, map[string][]health.Status{"shop/api": {inv, inv, inv}})
	sw, n := h.d.Sweep, h.d.Notifier
	h.at(0).due()
	h.at(DefaultSweepInterval).due()

	// Restart an hour later: no immediate sweep, the series goes on.
	h.restartWithSweep(sw, n)
	if next := h.at(DefaultSweepInterval + time.Hour).due(); !next.Equal(t0.Add(2 * DefaultSweepInterval)) {
		t.Errorf("after a restart, next sweep %s", next)
	}
	h.at(2 * DefaultSweepInterval).due()
	if got := tg.titles(); got != "[warning] shop/api: investigate for 3 sweeps in a row" {
		t.Errorf("messages %q: the series must survive the restart", got)
	}
	if prev := chk.prevs["shop/api"][2]; prev == nil || prev.Restarts["shop/api-pod"] != 2 {
		t.Errorf("restart counts not restored: %+v", prev)
	}
}

func TestSweepCheckErrorSkipsTheService(t *testing.T) {
	h, chk, _ := sweepHarness(t, nil)
	chk.fail = map[string]bool{"shop/api": true}
	h.at(0).due()
	if n := h.count(`"verdict"`); n != 1 {
		t.Errorf("%d sweep records, want only shop/web's", n)
	}
	chk.fail = nil
	h.at(DefaultSweepInterval).due()
	if prev := chk.prevs["shop/api"][1]; prev != nil {
		t.Errorf("a failed sweep must not become the previous one: %+v", prev)
	}
}

func TestVerdictsAPIIncludesSweeps(t *testing.T) {
	h, _, _ := sweepHarness(t, map[string][]health.Status{"shop/web": {health.StatusInvestigate}})
	h.at(0).due()
	api := NewAPI(h.d.Journal, "", h.clock.Now)
	_, body := get(t, api, "/v1/verdicts?service=shop/web", "")
	if len(body.Verdicts) != 1 {
		t.Fatalf("verdicts %+v", body.Verdicts)
	}
	v := body.Verdicts[0]
	if v.Source != "sweep" || v.Verdict != "investigate" || v.Checkpoint != "" || v.Revision != "3" {
		t.Errorf("sweep verdict %+v", v)
	}
}

func TestSweepDefaults(t *testing.T) {
	var c SweepConfig
	if c.interval() != 4*time.Hour {
		t.Errorf("default interval %s", c.interval())
	}
	if _, n := sweep.Step(sweep.State{}, health.StatusInvestigate, c.State); n != nil {
		t.Error("an isolated sweep warning is not notified by default")
	}
}

func keysOf(m map[string][]*health.PreviousSweep) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
