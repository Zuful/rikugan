package loki

import (
	"strings"
	"testing"
	"time"
)

func TestParseNanoTimestamp(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{
			name: "valid nanosecond epoch",
			in:   "1747930338000000000",
			want: time.Unix(0, 1747930338000000000),
		},
		{
			name: "zero",
			in:   "0",
			want: time.Unix(0, 0),
		},
		{
			name: "empty returns zero time",
			in:   "",
			want: time.Time{},
		},
		{
			name: "non-numeric returns zero time",
			in:   "not-a-number",
			want: time.Time{},
		},
		{
			name: "overflow returns zero time",
			in:   "99999999999999999999999999",
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseNanoTimestamp(tt.in)
			if !got.Equal(tt.want) {
				t.Errorf("parseNanoTimestamp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "empty map returns empty string",
			labels: map[string]string{},
			want:   "",
		},
		{
			name:   "nil map returns empty string",
			labels: nil,
			want:   "",
		},
		{
			name:   "single label",
			labels: map[string]string{"app": "payments"},
			want:   "app=payments",
		},
		{
			name:   "labels sorted by key (stable output for snapshot/test assertions)",
			labels: map[string]string{"namespace": "prod", "app": "payments", "pod": "payments-0"},
			want:   "app=payments, namespace=prod, pod=payments-0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatLabels(tt.labels); got != tt.want {
				t.Errorf("formatLabels = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildLabelQuery(t *testing.T) {
	tests := []struct {
		name      string
		service   string
		namespace string
		want      string
	}{
		{
			name:    "service only",
			service: "payments",
			want:    `{app="payments"}`,
		},
		{
			name:      "service and namespace",
			service:   "payments",
			namespace: "prod",
			want:      `{app="payments",namespace="prod"}`,
		},
		{
			name:      "namespace only is legal LogQL",
			namespace: "prod",
			want:      `{namespace="prod"}`,
		},
		{
			name: "both empty produces an empty selector",
			want: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := buildLabelQuery(tt.service, tt.namespace); got != tt.want {
				t.Errorf("buildLabelQuery(%q, %q) = %q, want %q",
					tt.service, tt.namespace, got, tt.want)
			}
		})
	}
}

func TestBuildSearchLogsReport_NoMatches(t *testing.T) {
	start := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	report := buildSearchLogsReport(`{app="x"} |= "boom"`, start, end, 100, nil)

	if !strings.Contains(report, `{app="x"} |= "boom"`) {
		t.Error("report should echo the query")
	}
	if !strings.Contains(report, "_No log lines found") {
		t.Errorf("expected empty-state message, got:\n%s", report)
	}
}

func TestBuildSearchLogsReport_RendersEntries(t *testing.T) {
	start := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	entries := []logEntry{
		{Timestamp: start, Labels: "app=payments", Line: "INFO request handled"},
		{Timestamp: start.Add(30 * time.Second), Labels: "app=payments", Line: "ERROR DB timeout"},
	}

	report := buildSearchLogsReport(`{app="payments"}`, start, end, 100, entries)

	// All entries should appear in the report.
	for _, e := range entries {
		if !strings.Contains(report, e.Line) {
			t.Errorf("expected entry %q in report, got:\n%s", e.Line, report)
		}
	}
	// Labels should appear at least once.
	if !strings.Contains(report, "app=payments") {
		t.Error("expected labels to appear in report")
	}
}
