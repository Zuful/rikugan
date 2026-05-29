package logs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// fakeLogs satisfies provider.LogsProvider; it records the call kind
// and returns whatever entries the test set up.
type fakeLogs struct {
	called  string
	lastQ   provider.LogQuery
	lastN   int
	entries []provider.LogEntry
	err     error
}

func (f *fakeLogs) Name() string                            { return "fake" }
func (f *fakeLogs) Tools() []provider.ToolDefinition        { return nil }

func (f *fakeLogs) GetLogs(_ context.Context, q provider.LogQuery) ([]provider.LogEntry, error) {
	f.called = "get"
	f.lastQ = q
	return f.entries, f.err
}

func (f *fakeLogs) TailLogs(_ context.Context, q provider.LogQuery, lines int) ([]provider.LogEntry, error) {
	f.called = "tail"
	f.lastQ = q
	f.lastN = lines
	return f.entries, f.err
}

func (f *fakeLogs) SearchLogs(_ context.Context, q provider.LogQuery) ([]provider.LogEntry, error) {
	f.called = "search"
	f.lastQ = q
	return f.entries, f.err
}

func TestConceptProvider_RegistersThreeCanonicalTools(t *testing.T) {
	t.Parallel()
	cp := NewConceptProvider(&fakeLogs{}, "fake", nil)
	got := make(map[string]bool)
	for _, td := range cp.Tools() {
		got[td.Tool.Name] = true
	}
	for _, want := range []string{"get_logs", "tail_logs", "search_logs"} {
		if !got[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
	if cp.Name() != "logs" {
		t.Errorf("Name: got %q, want \"logs\"", cp.Name())
	}
}

func TestBuildLogsReport_RendersHeaderAndTable(t *testing.T) {
	t.Parallel()
	entries := []provider.LogEntry{
		{
			Time:    time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			Labels:  map[string]string{"app": "checkout", "level": "error"},
			Message: "payment | failed",
		},
	}
	q := provider.LogQuery{
		Service:   "checkout",
		Namespace: "prod",
		Start:     time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	report := buildLogsReport("Logs", "loki", q, entries)

	for _, want := range []string{
		"## Logs",
		"**Backend:** `loki`",
		"**Service:** `checkout`",
		"**Namespace:** `prod`",
		"**Lines returned:** 1",
		"| Timestamp | Stream | Log Line |",
		"app=checkout, level=error",
		`payment \| failed`, // pipe must be escaped to not break the table
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n----\n%s\n----", want, report)
		}
	}
}

func TestBuildLogsReport_EmptyEntriesProducesNoneMarker(t *testing.T) {
	t.Parallel()
	report := buildLogsReport("Search Logs", "loki", provider.LogQuery{
		Match: "boom",
		Start: time.Now().Add(-time.Hour),
		End:   time.Now(),
	}, nil)
	if !strings.Contains(report, "_No log lines found._") {
		t.Errorf("expected empty-state marker, got:\n%s", report)
	}
	if !strings.Contains(report, "**Query/Match:** `boom`") {
		t.Errorf("expected match echoed in header, got:\n%s", report)
	}
}

func TestCompactLabels_DeterministicOrder(t *testing.T) {
	t.Parallel()
	got := compactLabels(map[string]string{"z": "1", "a": "2", "m": "3"})
	want := "a=2, m=3, z=1"
	if got != want {
		t.Errorf("compactLabels: got %q, want %q", got, want)
	}
}
