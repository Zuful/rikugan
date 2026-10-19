package watch

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

// A node or a shared dependency goes down: fifteen services degraded in
// one sweep send one message, not fifteen criticals at 4 a.m.
func TestSweepIncidentsAreGrouped(t *testing.T) {
	queue := map[string][]health.Status{}
	var services []provider.Service
	for i := range 15 {
		svc := svcIn("shop", fmt.Sprintf("svc%02d", i))
		services = append(services, svc)
		queue[serviceKey(svc)] = []health.Status{health.StatusDegraded}
	}
	h, _, tg := sweepHarness(t, queue)
	h.d.Sweep.Namespaces = []string{"shop"}
	h.d.Sweep.Services = &fakeServices{byNS: map[string][]provider.Service{"shop": services}}
	for _, svc := range services {
		h.source.bySvc[serviceKey(svc)] = provider.Rollout{State: provider.RolloutComplete, CompletedAt: t0.Add(-72 * time.Hour)}
	}
	h.due()
	if got := tg.titles(); got != "[critical] 15 services degraded (periodic sweep)" {
		t.Fatalf("messages:\n%s", got)
	}
	body := tg.msgs[0].Body
	for _, want := range []string{"shop/svc00\n  - memory_leak: degraded", "shop/svc14"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if n := h.count(`"title":"15 services degraded (periodic sweep)"`); n != 15 {
		t.Errorf("%d sweep records name the grouped message, want 15", n)
	}
}

// The grouped message no channel takes is retried as one message too.
func TestUndeliveredGroupIsRetriedAsOne(t *testing.T) {
	h, _, tg := sweepHarness(t, map[string][]health.Status{
		"shop/api": {health.StatusDegraded, health.StatusDegraded},
		"shop/web": {health.StatusDegraded, health.StatusDegraded},
	})
	tg.fails = -1
	h.d.Sweep.Namespaces = []string{"shop"}
	h.due()
	tg.mu.Lock()
	tg.fails = 0
	tg.mu.Unlock()
	h.at(time.Minute).due()
	if got := tg.titles(); got != "[critical] 2 services degraded (periodic sweep)" {
		t.Errorf("messages:\n%s", got)
	}
}

// The journal rotates while a service is in incident: after a restart
// it is still in incident, and the next degraded sweep sends nothing.
func TestSweepStateSurvivesRotation(t *testing.T) {
	deg := health.StatusDegraded
	h, _, tg := sweepHarness(t, map[string][]health.Status{"shop/api": {deg, deg, deg, deg}})
	h.maxSize = 300 // a rotation every record or two
	h.restartWithSweep(h.d.Sweep, h.d.Notifier)
	for i := range 3 {
		h.at(time.Duration(i) * DefaultSweepInterval).due()
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	rotated := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "verdicts-") {
			rotated++
		}
	}
	if rotated < 2 {
		t.Fatalf("%d rotated files, want the incident's record rotated away", rotated)
	}

	h.restartWithSweep(h.d.Sweep, h.d.Notifier)
	h.at(3 * DefaultSweepInterval).due()
	if got := tg.titles(); got != "[critical] shop/api: degraded (periodic sweep)" {
		t.Errorf("messages:\n%s\nwant the one critical, none after the restart", got)
	}
}
