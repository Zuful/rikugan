package loki

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyLevel(t *testing.T) {
	tests := []struct {
		name string
		line string
		want logLevel
	}{
		{"plain ERROR", "ERROR: something broke", levelError},
		{"lower error", "this is an error message", levelError},
		{"FATAL maps to error", "FATAL exit code 9", levelError},
		{"PANIC maps to error", "PANIC: runtime", levelError},
		{"WARN", "WARN connection retry", levelWarn},
		{"warning lower-case substring", "warning: deprecated api", levelWarn},
		{"INFO", "INFO server started on :8080", levelInfo},
		{"DEBUG", "DEBUG cache hit", levelDebug},
		{"TRACE maps to debug", "TRACE entering handler", levelDebug},
		{"unknown stays other", "started request 0xfff", levelOther},
		{"empty stays other", "", levelOther},
		{"error wins over warn when both present", "ERROR fallback after warn", levelError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLevel(tt.line); got != tt.want {
				t.Errorf("classifyLevel(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestNormalizePattern(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "iso timestamp stripped",
			line: "2026-05-22T14:32:18Z error opening file",
			want: "<TS> error opening file",
		},
		{
			name: "uuid stripped",
			line: "request 550e8400-e29b-41d4-a716-446655440000 failed",
			want: "request <UUID> failed",
		},
		{
			name: "word-boundary integers replaced (200ms keeps the trailing 'ms' attached so isn't bounded)",
			line: "retry attempt 7 after 200ms",
			want: "retry attempt <N> after 200ms",
		},
		{
			name: "space-bounded integers replaced",
			line: "retry attempt 7 after 30 seconds",
			want: "retry attempt <N> after <N> seconds",
		},
		{
			name: "no replacements when nothing matches",
			line: "plain message with no dynamic content",
			want: "plain message with no dynamic content",
		},
		{
			name: "timestamp + uuid + numbers all replaced",
			line: "2026-05-22T14:32:18.000Z trace-id=550e8400-e29b-41d4-a716-446655440000 status=500",
			want: "<TS> trace-id=<UUID> status=<N>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizePattern(tt.line); got != tt.want {
				t.Errorf("normalizePattern(%q)\n got  = %q\n want = %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestNormalizePattern_TruncatesLongLines(t *testing.T) {
	long := strings.Repeat("x", maxLogLineLen+200)
	got := normalizePattern(long)
	if len(got) != maxLogLineLen {
		t.Fatalf("expected truncation to %d chars, got %d", maxLogLineLen, len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated line to end with '...', got %q", got[len(got)-10:])
	}
}

func TestGroupErrorPatterns(t *testing.T) {
	t0 := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Second)
	t2 := t0.Add(30 * time.Second)

	entries := []logEntry{
		{Timestamp: t0, Line: "ERROR connection refused 192.168.1.10:5432"},
		{Timestamp: t1, Line: "ERROR connection refused 192.168.1.10:5432"},
		{Timestamp: t2, Line: "ERROR connection refused 192.168.1.11:5432"},
		{Timestamp: t0, Line: "INFO request handled"},     // not error → ignored
		{Timestamp: t1, Line: "WARN slow query (1500ms)"}, // not error → ignored
		{Timestamp: t2, Line: "ERROR disk full"},
	}

	patterns := groupErrorPatterns(entries)

	// Two normalized patterns:
	//   "ERROR connection refused <N>.<N>.<N>.<N>:<N>" (3 occurrences)
	//   "ERROR disk full"                              (1 occurrence)
	if len(patterns) != 2 {
		t.Fatalf("expected 2 patterns, got %d: %+v", len(patterns), patterns)
	}
	// Sorted by Count desc — the connection-refused group comes first.
	if patterns[0].Count != 3 {
		t.Errorf("expected top pattern Count=3, got %d", patterns[0].Count)
	}
	if !strings.Contains(patterns[0].Pattern, "connection refused") {
		t.Errorf("expected top pattern to be the connection-refused group, got %q", patterns[0].Pattern)
	}
	if !patterns[0].FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen = %v, want %v", patterns[0].FirstSeen, t0)
	}
	if !patterns[0].LastSeen.Equal(t2) {
		t.Errorf("LastSeen = %v, want %v", patterns[0].LastSeen, t2)
	}
}

func TestBuildAnomalyReport_EmptyEntries(t *testing.T) {
	report := buildAnomalyReport("payments", "prod", time.Hour, nil)
	if !strings.Contains(report, "payments") {
		t.Error("report should mention the service name")
	}
	if !strings.Contains(report, "_No log lines found") {
		t.Errorf("expected empty-state message, got:\n%s", report)
	}
}

func TestBuildAnomalyReport_OmitsNamespaceLineWhenEmpty(t *testing.T) {
	report := buildAnomalyReport("payments", "", time.Hour, nil)
	if strings.Contains(report, "**Namespace:**") {
		t.Errorf("namespace line should be omitted when empty, got:\n%s", report)
	}
}
