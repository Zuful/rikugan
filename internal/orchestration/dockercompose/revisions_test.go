package dockercompose

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

const stHealthy = `{"Status":"running","Running":true,"StartedAt":"2026-10-18T10:00:05Z","FinishedAt":"0001-01-01T00:00:00Z",
	"Health":{"Status":"healthy","FailingStreak":0,"Log":[
		{"Start":"2026-10-18T10:00:10Z","ExitCode":1},
		{"Start":"2026-10-18T10:00:20Z","ExitCode":0},
		{"Start":"2026-10-18T10:00:30Z","ExitCode":0}]}}`

func TestRevisions(t *testing.T) {
	r := &inspectRunner{
		ps: strings.Join([]string{psLine("o1", "web-api-1", "api"), psLine("n1", "web-api-2", "api"), psLine("n2", "web-api-3", "api"), psLine("w1", "web-worker-1", "worker")}, "\n"),
		inspects: map[string]string{
			"o1": inspectJSON("o1", "web-api-1", "2026-10-17T08:00:00Z", "old", 0, stExited),
			"n1": inspectJSON("n1", "web-api-2", "2026-10-18T10:00:00Z", "new", 0, stHealthy),
			"n2": inspectJSON("n2", "web-api-3", "2026-10-18T10:00:02Z", "new", 0, stRunning),
		},
	}
	revs, err := New(WithRunner(r)).Revisions(context.Background(), provider.Service{Name: "api", Namespace: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[0].Hash != "new" || revs[1].Hash != "old" {
		t.Fatalf("revisions %+v, want new then old", revs)
	}
	n := revs[0]
	if n.Containers != 2 || n.Ready != 2 || n.Live != 2 || !n.Created.Equal(time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("new revision %+v", n)
	}
	// n1 healthy since its first passing probe (10:00:20), n2 ready
	// since it started (10:00:05, no healthcheck): the later one.
	if want := time.Date(2026, 10, 18, 10, 0, 20, 0, time.UTC); !n.ReadyAt.Equal(want) {
		t.Errorf("ReadyAt %s, want %s", n.ReadyAt, want)
	}
	if o := revs[1]; o.Live != 0 || o.Ready != 0 || !o.ReadyAt.IsZero() {
		t.Errorf("old revision %+v", o)
	}
}

func TestRevisionsNotAllReady(t *testing.T) {
	r := &inspectRunner{
		ps: strings.Join([]string{psLine("n1", "web-api-1", "api"), psLine("n2", "web-api-2", "api")}, "\n"),
		inspects: map[string]string{
			"n1": inspectJSON("n1", "web-api-1", "2026-10-18T10:00:00Z", "new", 0, stHealthy),
			"n2": inspectJSON("n2", "web-api-2", "2026-10-18T10:00:00Z", "new", 0, stStarting),
		},
	}
	revs, err := New(WithRunner(r)).Revisions(context.Background(), provider.Service{Name: "api", Namespace: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 || revs[0].Ready != 1 || !revs[0].ReadyAt.IsZero() {
		t.Errorf("revisions %+v: ReadyAt must stay zero until every container is ready", revs)
	}
}

// Docker keeps the last 5 probes. A log full of passing probes has lost
// the start of the run: the container's start stands in for it, so the
// time does not move from one call to the next.
func TestReadyAtWithATruncatedHealthLog(t *testing.T) {
	probes := func(starts ...string) string {
		var out []string
		for _, s := range starts {
			out = append(out, `{"Start":"2026-10-18T10:`+s+`Z","ExitCode":0}`)
		}
		return `{"Status":"running","Running":true,"StartedAt":"2026-10-18T10:00:05Z","FinishedAt":"0001-01-01T00:00:00Z",
	"Health":{"Status":"healthy","FailingStreak":0,"Log":[` + strings.Join(out, ",") + `]}}`
	}
	for _, tc := range []struct {
		name      string
		state     string
		want      string
		estimated bool
	}{
		{"four passing probes, nothing lost", probes("00:30", "01:00", "01:30", "02:00"), "10:00:30", false},
		{"five passing probes: the first may be lost", probes("01:00", "01:30", "02:00", "02:30", "03:00"), "10:00:05", true},
		{"same, two probes later: the time does not move", probes("02:00", "02:30", "03:00", "03:30", "04:00"), "10:00:05", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &inspectRunner{
				ps:       psLine("n1", "web-api-1", "api"),
				inspects: map[string]string{"n1": inspectJSON("n1", "web-api-1", "2026-10-18T10:00:00Z", "new", 0, tc.state)},
			}
			revs, err := New(WithRunner(r)).Revisions(context.Background(), provider.Service{Name: "api", Namespace: "web"})
			if err != nil {
				t.Fatal(err)
			}
			if got := revs[0].ReadyAt.Format("15:04:05"); got != tc.want || revs[0].ReadyAtEstimated != tc.estimated {
				t.Errorf("ReadyAt %s (estimated %v), want %s (estimated %v)", got, revs[0].ReadyAtEstimated, tc.want, tc.estimated)
			}
		})
	}
}
