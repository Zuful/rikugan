package dockercompose

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// logsRunner answers docker compose ps with two web containers and a
// db one, and docker logs with each container's canned output; it
// records the docker logs calls.
type logsRunner struct {
	stubRunner
	logs     map[string]string // container ID → output
	combined [][]string
}

func (r *logsRunner) RunCombined(_ context.Context, name string, args ...string) ([]byte, error) {
	r.combined = append(r.combined, append([]string{name}, args...))
	return []byte(r.logs[args[len(args)-1]]), nil
}

func newLogsRunner() *logsRunner {
	return &logsRunner{
		stubRunner: stubRunner{responses: map[string]string{"compose": `{"ID":"w1","Name":"shop-web-1","Project":"shop","Service":"web","State":"running"}
{"ID":"w2","Name":"shop-web-2","Project":"shop","Service":"web","State":"running"}
{"ID":"d1","Name":"shop-db-1","Project":"shop","Service":"db","State":"running"}`}},
		logs: map[string]string{
			"w1": "2026-10-24T12:00:01.000000001Z GET /pay 200\n2026-10-24T12:00:03.5Z payment failed: upstream returned 500\n",
			"w2": "2026-10-24T12:00:02Z GET /pay 200\n2026-10-24T12:00:04Z PAYMENT FAILED: timeout\n  at charge()\n",
			"d1": "2026-10-24T12:00:05Z checkpoint complete\n",
		},
	}
}

func TestGetLogsMergesTheServiceContainers(t *testing.T) {
	r := newLogsRunner()
	p := New(WithRunner(r), WithProject("shop"))
	start := time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)
	entries, err := p.GetLogs(context.Background(), provider.LogQuery{Service: "web", Start: start, End: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Labels["container"]+": "+e.Message)
	}
	want := []string{
		"shop-web-2: PAYMENT FAILED: timeout",
		"shop-web-2:   at charge()", // no time of its own: timed like the line before
		"shop-web-1: payment failed: upstream returned 500",
		"shop-web-2: GET /pay 200",
		"shop-web-1: GET /pay 200",
	}
	if !slices.Equal(got, want) {
		t.Errorf("entries, newest first:\n got %q\nwant %q", got, want)
	}
	if len(r.combined) != 2 {
		t.Fatalf("docker logs calls = %v, want one per web container", r.combined)
	}
	call := strings.Join(r.combined[0], " ")
	if want := "docker logs --timestamps --tail 5000 --since 2026-10-24T11:00:00Z --until 2026-10-24T12:00:00Z -- w1"; call != want {
		t.Errorf("call = %q, want %q", call, want)
	}
}

func TestSearchLogsMatchesCaseAside(t *testing.T) {
	p := New(WithRunner(newLogsRunner()), WithProject("shop"))
	entries, err := p.SearchLogs(context.Background(), provider.LogQuery{Match: "payment failed", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != "PAYMENT FAILED: timeout" {
		t.Errorf("entries = %+v, want the newest match alone", entries)
	}
	if _, err := p.SearchLogs(context.Background(), provider.LogQuery{Service: "web"}); err == nil {
		t.Error("a search without a match should fail")
	}
}

func TestTailLogsAsksDockerForTheLastLines(t *testing.T) {
	r := newLogsRunner()
	p := New(WithRunner(r), WithProject("shop"))
	entries, err := p.TailLogs(context.Background(), provider.LogQuery{Service: "db"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Labels["service"] != "db" {
		t.Errorf("entries = %+v", entries)
	}
	if call := strings.Join(r.combined[0], " "); call != "docker logs --timestamps --tail 20 -- d1" {
		t.Errorf("call = %q", call)
	}
}

func TestLogsRejectsAFlagAsService(t *testing.T) {
	p := New(WithRunner(newLogsRunner()), WithProject("shop"))
	if _, err := p.GetLogs(context.Background(), provider.LogQuery{Service: "-f"}); err == nil {
		t.Error("a service name shaped like a flag should be refused")
	}
}
