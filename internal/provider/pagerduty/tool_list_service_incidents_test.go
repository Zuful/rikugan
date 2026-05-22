package pagerduty

import (
	"strings"
	"testing"
	"time"
)

func TestParseFlexibleDuration(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "hours", in: "3h", want: 3 * time.Hour},
		{name: "minutes", in: "90m", want: 90 * time.Minute},
		{name: "days suffix", in: "7d", want: 7 * 24 * time.Hour},
		{name: "single-day suffix", in: "1d", want: 24 * time.Hour},
		{name: "zero-day suffix", in: "0d", want: 0},
		{name: "compound stdlib duration", in: "1h30m", want: 90 * time.Minute},
		{name: "trailing whitespace stripped", in: "3h  ", want: 3 * time.Hour},
		{name: "leading whitespace stripped", in: "  3h", want: 3 * time.Hour},
		{name: "empty string is invalid", in: "", wantErr: true},
		{name: "bad units rejected", in: "5xyz", wantErr: true},
		{name: "non-numeric days rejected", in: "xd", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlexibleDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFlexibleDuration(%q) returned nil error, want error (got %v)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlexibleDuration(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseFlexibleDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildServiceIncidentsReport_EmptyWindow(t *testing.T) {
	report := buildServiceIncidentsReport("checkout", 24*time.Hour, nil)
	if !strings.Contains(report, "checkout") {
		t.Error("expected service name in report")
	}
	if !strings.Contains(report, "service has been quiet") {
		t.Errorf("expected empty-state message, got:\n%s", report)
	}
}

func TestBuildServiceIncidentsReport_StatusBreakdown(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	incidents := []Incident{
		{IncidentNumber: 1, Title: "DB timeout", Status: "resolved", Urgency: "high", CreatedAt: now},
		{IncidentNumber: 2, Title: "Cache miss spike", Status: "resolved", Urgency: "low", CreatedAt: now},
		{IncidentNumber: 3, Title: "Payment 5xx", Status: "acknowledged", Urgency: "high", CreatedAt: now},
		{IncidentNumber: 4, Title: "Auth latency", Status: "triggered", Urgency: "high", CreatedAt: now},
	}

	report := buildServiceIncidentsReport("checkout", 24*time.Hour, incidents)

	// Status breakdown must list each non-zero status with its count.
	if !strings.Contains(report, "triggered=1") {
		t.Errorf("missing triggered=1 in breakdown:\n%s", report)
	}
	if !strings.Contains(report, "acknowledged=1") {
		t.Errorf("missing acknowledged=1 in breakdown:\n%s", report)
	}
	if !strings.Contains(report, "resolved=2") {
		t.Errorf("missing resolved=2 in breakdown:\n%s", report)
	}
	// Every incident title should appear in the table.
	for _, i := range incidents {
		if !strings.Contains(report, i.Title) {
			t.Errorf("missing incident title %q in report", i.Title)
		}
	}
}

func TestBuildServiceIncidentsReport_OmitsZeroStatusesFromBreakdown(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	incidents := []Incident{
		{IncidentNumber: 1, Title: "Only resolved", Status: "resolved", Urgency: "low", CreatedAt: now},
	}

	report := buildServiceIncidentsReport("checkout", time.Hour, incidents)

	if strings.Contains(report, "triggered=") {
		t.Errorf("triggered=0 should be omitted, got:\n%s", report)
	}
	if strings.Contains(report, "acknowledged=") {
		t.Errorf("acknowledged=0 should be omitted, got:\n%s", report)
	}
	if !strings.Contains(report, "resolved=1") {
		t.Errorf("resolved count should appear, got:\n%s", report)
	}
}
