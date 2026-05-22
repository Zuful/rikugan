package sentry

import (
	"strings"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under limit", "short", 10, "short"},
		{"exactly at limit", "1234567890", 10, "1234567890"},
		{"over limit gets ellipsis", "12345678901234", 10, "1234567..."},
		{"max of 4 leaves room for one char + ellipsis", "abcdef", 4, "a..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := truncate(tt.in, tt.max); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestBuildIssuesListReport_EmptyShowsFriendlyMessage(t *testing.T) {
	report := buildIssuesListReport("web", "is:unresolved", nil)
	if !strings.Contains(report, "_No issues match the query._") {
		t.Errorf("expected empty-state message, got:\n%s", report)
	}
}

func TestBuildIssuesListReport_DefaultQueryNoted(t *testing.T) {
	report := buildIssuesListReport("web", "", nil)
	if !strings.Contains(report, "is:unresolved` (default)") {
		t.Errorf("expected default-query annotation when query is empty, got:\n%s", report)
	}
}

func TestBuildIssuesListReport_ExplicitQueryEchoed(t *testing.T) {
	report := buildIssuesListReport("web", "is:unresolved level:error", nil)
	if !strings.Contains(report, "is:unresolved level:error") {
		t.Errorf("expected explicit query to be echoed, got:\n%s", report)
	}
	if strings.Contains(report, "(default)") {
		t.Errorf("default annotation should not appear when an explicit query was passed:\n%s", report)
	}
}

func TestBuildIssuesListReport_RendersIssueRows(t *testing.T) {
	ts := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	issues := []Issue{
		{
			ShortID:   "WEB-A12",
			Title:     "panic: nil pointer dereference",
			Level:     "error",
			Count:     "42",
			UserCount: 3,
			LastSeen:  ts,
		},
		{
			ShortID:   "WEB-B34",
			Title:     "context deadline exceeded calling /api/checkout",
			Level:     "warning",
			Count:     "5",
			UserCount: 1,
			LastSeen:  ts.Add(time.Minute),
		},
	}

	report := buildIssuesListReport("web", "", issues)

	// Short IDs appear in the table as code-fenced tokens.
	for _, want := range []string{"WEB-A12", "WEB-B34"} {
		if !strings.Contains(report, want) {
			t.Errorf("expected short ID %q in report", want)
		}
	}
	// Titles appear too.
	for _, want := range []string{"panic: nil pointer dereference"} {
		if !strings.Contains(report, want) {
			t.Errorf("expected title %q in report", want)
		}
	}
	// Counts surface verbatim.
	if !strings.Contains(report, "| 42 |") {
		t.Errorf("expected occurrence count 42 in report:\n%s", report)
	}
}
