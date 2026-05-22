package sentry

import (
	"strings"
	"testing"
	"time"
)

func newTestIssue() *Issue {
	ts := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	return &Issue{
		ID:        "987654321",
		ShortID:   "WEB-A12",
		Title:     "panic: nil pointer dereference",
		Level:     "error",
		Status:    "unresolved",
		Project:   Project{Slug: "web", Name: "Web App"},
		Count:     "42",
		UserCount: 3,
		FirstSeen: ts,
		LastSeen:  ts.Add(time.Hour),
	}
}

func TestBuildIssueDetailReport_AlwaysPresentFields(t *testing.T) {
	report := buildIssueDetailReport(newTestIssue())

	mustContain := []string{
		"## Sentry Issue `WEB-A12`",          // header
		"`987654321`",                        // numeric ID
		"| Level | error |",                  // level row
		"| Status | unresolved |",            // status row
		"| Project | web |",                  // project slug
		"| Count | 42 |",                     // count
		"| Users affected | 3 |",             // users
	}
	for _, want := range mustContain {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in report:\n%s", want, report)
		}
	}
}

func TestBuildIssueDetailReport_OmitsCulpritWhenEmpty(t *testing.T) {
	i := newTestIssue()
	i.Culprit = ""

	report := buildIssueDetailReport(i)
	if strings.Contains(report, "| Culprit |") {
		t.Errorf("Culprit row should be omitted when empty:\n%s", report)
	}
}

func TestBuildIssueDetailReport_RenderCulpritWhenSet(t *testing.T) {
	i := newTestIssue()
	i.Culprit = "main.handleCheckout"

	report := buildIssueDetailReport(i)
	if !strings.Contains(report, "| Culprit | main.handleCheckout |") {
		t.Errorf("Culprit row missing:\n%s", report)
	}
}

func TestBuildIssueDetailReport_FailureMetadataSection(t *testing.T) {
	i := newTestIssue()
	i.Metadata = Metadata{
		Type:     "RuntimeError",
		Value:    "nil pointer dereference",
		Function: "main.handleCheckout",
		Filename: "main.go",
	}

	report := buildIssueDetailReport(i)

	if !strings.Contains(report, "### Failure metadata") {
		t.Errorf("expected Failure metadata section:\n%s", report)
	}
	for _, want := range []string{"RuntimeError", "nil pointer dereference", "main.handleCheckout", "main.go"} {
		if !strings.Contains(report, want) {
			t.Errorf("missing metadata field %q in report:\n%s", want, report)
		}
	}
}

func TestBuildIssueDetailReport_NoMetadataSectionWhenEmpty(t *testing.T) {
	i := newTestIssue() // Metadata zero-value
	report := buildIssueDetailReport(i)
	if strings.Contains(report, "### Failure metadata") {
		t.Errorf("Failure metadata section should be omitted when empty:\n%s", report)
	}
}

func TestBuildIssueDetailReport_FirstReleaseHintBridgesToGitProviders(t *testing.T) {
	i := newTestIssue()
	i.FirstRelease = &Release{
		Version: "v1.2.3",
		Created: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC),
	}

	report := buildIssueDetailReport(i)

	if !strings.Contains(report, "| First release |") {
		t.Errorf("First release row should appear:\n%s", report)
	}
	if !strings.Contains(report, "`v1.2.3`") {
		t.Errorf("release version should be code-fenced in the row:\n%s", report)
	}
	// The bridging hint points the agent at get_commit_diff in the github/gitlab providers.
	if !strings.Contains(report, "get_commit_diff") {
		t.Errorf("expected bridging hint mentioning get_commit_diff, got:\n%s", report)
	}
}

func TestBuildIssueDetailReport_NoReleaseNoHint(t *testing.T) {
	i := newTestIssue()
	i.FirstRelease = nil

	report := buildIssueDetailReport(i)
	if strings.Contains(report, "First release") {
		t.Errorf("First release row should be omitted when nil:\n%s", report)
	}
	if strings.Contains(report, "get_commit_diff") {
		t.Errorf("bridging hint should be omitted when no release attribution:\n%s", report)
	}
}
