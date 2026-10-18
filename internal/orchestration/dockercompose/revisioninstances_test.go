package dockercompose

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// inspectRunner answers `docker compose ps` with a fixed listing, and
// `docker inspect <id>` and `docker events --filter container=<id>` per
// container id, which the shared stubRunner (keyed by subcommand only)
// cannot do.
type inspectRunner struct {
	ps       string
	inspects map[string]string
	events   map[string]string
}

func (r *inspectRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	switch {
	case len(args) > 0 && args[0] == "compose":
		return []byte(r.ps), nil
	case len(args) == 2 && args[0] == "inspect":
		if out, ok := r.inspects[args[1]]; ok {
			return []byte(out), nil
		}
		return nil, fmt.Errorf("no such container %s", args[1])
	case len(args) > 0 && args[0] == "events":
		for _, a := range args {
			if id, ok := strings.CutPrefix(a, "container="); ok {
				return []byte(r.events[id]), nil
			}
		}
	}
	return nil, nil
}

func psLine(id, name, service string) string {
	return fmt.Sprintf(`{"ID":%q,"Name":%q,"Project":"web","Service":%q,"State":"running"}`, id, name, service)
}

// inspectJSON renders a docker inspect array for one container. state
// is the raw JSON of the State object.
func inspectJSON(id, name, created, hash string, restarts int, state string) string {
	return fmt.Sprintf(`[{"Id":%q,"Name":"/%s","Created":%q,"RestartCount":%d,"State":%s,
		"Config":{"Image":"api:1","Labels":{%q:%q}}}]`, id, name, created, restarts, state, labelConfigHash, hash)
}

const (
	stRunning = `{"Status":"running","Running":true,"StartedAt":"2026-10-18T10:00:05Z","FinishedAt":"0001-01-01T00:00:00Z"}`
	// Running again after a restart: Docker has reset ExitCode and
	// OOMKilled, only FinishedAt still describes the previous run.
	stRestarted = `{"Status":"running","Running":true,"ExitCode":0,"OOMKilled":false,"StartedAt":"2026-10-18T10:03:00Z","FinishedAt":"2026-10-18T10:02:58Z"}`
	stLooping   = `{"Status":"restarting","Running":true,"Restarting":true,"ExitCode":1,"StartedAt":"2026-10-18T10:04:00Z","FinishedAt":"2026-10-18T10:04:30Z"}`
	stUnhealthy = `{"Status":"running","Running":true,"StartedAt":"2026-10-18T10:00:05Z","FinishedAt":"0001-01-01T00:00:00Z",
		"Health":{"Status":"unhealthy","FailingStreak":3,"Log":[
			{"Start":"2026-10-18T10:05:00Z","ExitCode":0},
			{"Start":"2026-10-18T10:05:30Z","ExitCode":1},
			{"Start":"2026-10-18T10:06:00Z","ExitCode":1},
			{"Start":"2026-10-18T10:06:30Z","ExitCode":1}]}}`
	stStarting = `{"Status":"running","Running":true,"StartedAt":"2026-10-18T10:00:05Z","FinishedAt":"0001-01-01T00:00:00Z",
		"Health":{"Status":"starting","FailingStreak":0,"Log":[]}}`
	stExited = `{"Status":"exited","Running":false,"ExitCode":2,"StartedAt":"2026-10-18T10:00:05Z","FinishedAt":"2026-10-18T10:01:00Z"}`
)

// The kernel's OOM notification precedes the exit it causes, within the
// sub-second; timeNano orders them.
const oomEvent = `{"Type":"container","Action":"oom","time":1792317777,"timeNano":1792317777900000000,"Actor":{"ID":"c1"}}`

func dieEvent(code int) string {
	return fmt.Sprintf(`{"Type":"container","Action":"die","time":1792317778,"timeNano":1792317778000000000,"Actor":{"ID":"c1","Attributes":{"exitCode":"%d"}}}`, code)
}

func TestExitsFromEvents_PairsEachOOMWithTheNextDie(t *testing.T) {
	raw := strings.Join([]string{
		`{"Action":"die","timeNano":3000,"Actor":{"Attributes":{"exitCode":"1"}}}`,
		`{"Action":"oom","timeNano":1000}`,
		`{"Action":"die","timeNano":2000,"Actor":{"Attributes":{"exitCode":"137"}}}`,
		`{"Action":"die","timeNano":4000,"Actor":{"Attributes":{"exitCode":"137"}}}`,
	}, "\n")
	got := exitsFromEvents([]byte(raw), "c")
	want := []struct {
		reason string
		oom    bool
	}{{"OOMKilled", true}, {"Error", false}, {"Killed", false}}
	if len(got) != len(want) {
		t.Fatalf("got %d terminations, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Reason != w.reason || got[i].OOM != w.oom {
			t.Errorf("termination %d: got %s oom=%v, want %s oom=%v", i, got[i].Reason, got[i].OOM, w.reason, w.oom)
		}
	}
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestGetRevisionInstances_DefaultsToNewestConfigHash(t *testing.T) {
	r := &inspectRunner{
		ps: strings.Join([]string{
			psLine("c-old", "web-api-1", "api"),
			psLine("c-new", "web-api-2", "api"),
			psLine("c-db", "web-db-1", "db"),
		}, "\n"),
		inspects: map[string]string{
			"c-old": inspectJSON("c-old", "web-api-1", "2026-10-17T09:00:00Z", "hash-old", 0, stRunning),
			"c-new": inspectJSON("c-new", "web-api-2", "2026-10-18T10:00:00Z", "hash-new", 0, stRunning),
			"c-db":  inspectJSON("c-db", "web-db-1", "2026-10-18T11:00:00Z", "hash-db", 0, stRunning),
		},
	}
	p := New(WithRunner(r))
	got, err := p.GetRevisionInstances(context.Background(), provider.Service{Name: "api", Namespace: "web"}, "")
	if err != nil {
		t.Fatalf("GetRevisionInstances: %v", err)
	}
	if len(got) != 1 || got[0].Name != "web-api-2" || got[0].Revision != "hash-new" || !got[0].Ready {
		t.Fatalf("want only the ready web-api-2 of hash-new (db's newer container is another service), got %+v", got)
	}

	old, err := p.GetRevisionInstances(context.Background(), provider.Service{Name: "api", Namespace: "web"}, "hash-old")
	if err != nil {
		t.Fatalf("GetRevisionInstances(hash-old): %v", err)
	}
	if len(old) != 1 || old[0].Name != "web-api-1" {
		t.Fatalf("want web-api-1 for hash-old, got %+v", old)
	}
}

func TestContainerInstance(t *testing.T) {
	created := "2026-10-18T10:00:00Z"
	tests := []struct {
		name   string
		state  string
		events string
		want   provider.Instance
	}{
		{
			name:  "running without healthcheck is ready",
			state: stRunning,
			want:  provider.Instance{Ready: true},
		},
		{
			name:   "OOM attested by the event log survives the restart",
			state:  stRestarted,
			events: oomEvent + "\n" + dieEvent(137),
			want: provider.Instance{Ready: true, RestartCount: 1, Terminations: []provider.Termination{
				{Container: "web-api-1", Time: at("2026-10-18T10:02:58Z"), Reason: "OOMKilled", ExitCode: 137, OOM: true},
			}},
		},
		{
			name:   "137 without an oom event is a kill",
			state:  stRestarted,
			events: dieEvent(137),
			want: provider.Instance{Ready: true, RestartCount: 1, Terminations: []provider.Termination{
				{Container: "web-api-1", Time: at("2026-10-18T10:02:58Z"), Reason: "Killed", ExitCode: 137},
			}},
		},
		{
			name:  "restarted with the events gone: unknown, not the clean exit inspect claims",
			state: stRestarted,
			want: provider.Instance{Ready: true, RestartCount: 1, Terminations: []provider.Termination{
				{Container: "web-api-1", Time: at("2026-10-18T10:02:58Z"), Reason: "Unknown", ExitCode: -1},
			}},
		},
		{
			name:  "restarting is crash looping and not ready since it last stopped",
			state: stLooping,
			want: provider.Instance{NotReadySince: at("2026-10-18T10:04:30Z"), RestartCount: 1, CrashLooping: true, Terminations: []provider.Termination{
				{Container: "web-api-1", Time: at("2026-10-18T10:04:30Z"), Reason: "Error", ExitCode: 1},
			}},
		},
		{
			name:  "unhealthy since the first probe of the failing streak",
			state: stUnhealthy,
			want:  provider.Instance{NotReadySince: at("2026-10-18T10:05:30Z")},
		},
		{
			name:  "healthcheck still starting: not ready since start",
			state: stStarting,
			want:  provider.Instance{NotReadySince: at("2026-10-18T10:00:05Z")},
		},
		{
			name:  "exited",
			state: stExited,
			want: provider.Instance{NotReadySince: at("2026-10-18T10:01:00Z"), Terminations: []provider.Termination{
				{Container: "web-api-1", Time: at("2026-10-18T10:01:00Z"), Reason: "Error", ExitCode: 2},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restarts := tc.want.RestartCount
			r := &inspectRunner{
				ps:       psLine("c1", "web-api-1", "api"),
				inspects: map[string]string{"c1": inspectJSON("c1", "web-api-1", created, "h", restarts, tc.state)},
				events:   map[string]string{"c1": tc.events},
			}
			got, err := New(WithRunner(r)).GetRevisionInstances(context.Background(), provider.Service{Name: "api", Namespace: "web"}, "h")
			if err != nil {
				t.Fatalf("GetRevisionInstances: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want 1 instance, got %d", len(got))
			}
			want := tc.want
			want.Name, want.Revision, want.Created = "web-api-1", "h", at(created)
			if !instanceEqual(got[0], want) {
				t.Errorf("got  %+v\nwant %+v", got[0], want)
			}
		})
	}
}

func instanceEqual(a, b provider.Instance) bool {
	if a.Name != b.Name || a.Revision != b.Revision || !a.Created.Equal(b.Created) || a.Ready != b.Ready ||
		!a.NotReadySince.Equal(b.NotReadySince) || a.RestartCount != b.RestartCount || a.CrashLooping != b.CrashLooping ||
		len(a.Terminations) != len(b.Terminations) {
		return false
	}
	for i := range a.Terminations {
		x, y := a.Terminations[i], b.Terminations[i]
		if x.Container != y.Container || !x.Time.Equal(y.Time) || x.Reason != y.Reason || x.ExitCode != y.ExitCode || x.OOM != y.OOM {
			return false
		}
	}
	return true
}
