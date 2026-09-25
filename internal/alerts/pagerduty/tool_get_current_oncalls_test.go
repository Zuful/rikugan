package pagerduty

import (
	"strings"
	"testing"
)

func TestFilterOncallsByPolicy(t *testing.T) {
	oncalls := []Oncall{
		{EscalationPolicy: Reference{Summary: "Payments Primary"}, User: Reference{Summary: "alice"}},
		{EscalationPolicy: Reference{Summary: "Payments Secondary"}, User: Reference{Summary: "bob"}},
		{EscalationPolicy: Reference{Summary: "Infra Primary"}, User: Reference{Summary: "carol"}},
		{EscalationPolicy: Reference{Summary: "billing"}, User: Reference{Summary: "dave"}},
	}

	tests := []struct {
		name         string
		filter       string
		wantUsers    []string
		wantCount    int
		wantEmptyAll bool
	}{
		{
			name:      "case-insensitive match returns matching oncalls",
			filter:    "payments",
			wantUsers: []string{"alice", "bob"},
		},
		{
			name:      "uppercase filter still matches lowercase policy",
			filter:    "BILLING",
			wantUsers: []string{"dave"},
		},
		{
			name:      "partial substring across policies",
			filter:    "primary",
			wantUsers: []string{"alice", "carol"},
		},
		{
			name:      "no match returns nil",
			filter:    "nonexistent",
			wantUsers: nil,
		},
		{
			name:      "empty filter matches everything (substring '' contained anywhere)",
			filter:    "",
			wantUsers: []string{"alice", "bob", "carol", "dave"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterOncallsByPolicy(oncalls, tt.filter)
			if len(got) != len(tt.wantUsers) {
				t.Fatalf("got %d oncalls, want %d (%+v)", len(got), len(tt.wantUsers), got)
			}
			for i, want := range tt.wantUsers {
				if got[i].User.Summary != want {
					t.Errorf("oncall %d user = %q, want %q", i, got[i].User.Summary, want)
				}
			}
		})
	}
}

func TestBuildOncallsReport_EmptyShowsFriendlyMessage(t *testing.T) {
	report := buildOncallsReport("", nil)
	if !strings.Contains(report, "_No on-call assignments match._") {
		t.Errorf("expected empty-state message, got:\n%s", report)
	}
}

func TestBuildOncallsReport_FilterEchoedWhenSet(t *testing.T) {
	report := buildOncallsReport("payments", nil)
	if !strings.Contains(report, "policy name contains `payments`") {
		t.Errorf("expected filter to be echoed, got:\n%s", report)
	}
}

func TestBuildOncallsReport_GroupsAndOrdersByEscalationLevel(t *testing.T) {
	oncalls := []Oncall{
		{
			EscalationPolicy: Reference{Summary: "Payments"},
			EscalationLevel:  2, // secondary
			User:             Reference{Summary: "bob"},
		},
		{
			EscalationPolicy: Reference{Summary: "Payments"},
			EscalationLevel:  1, // primary
			User:             Reference{Summary: "alice"},
		},
		{
			EscalationPolicy: Reference{Summary: "Infra"},
			EscalationLevel:  1,
			User:             Reference{Summary: "carol"},
		},
	}

	report := buildOncallsReport("", oncalls)

	// Two distinct policy headers (sorted alphabetically: Infra before Payments).
	infraIdx := strings.Index(report, "### Infra")
	paymentsIdx := strings.Index(report, "### Payments")
	if infraIdx == -1 || paymentsIdx == -1 {
		t.Fatalf("missing policy headers:\n%s", report)
	}
	if infraIdx >= paymentsIdx {
		t.Errorf("policies not sorted alphabetically: Infra index=%d, Payments index=%d", infraIdx, paymentsIdx)
	}

	// Within Payments, alice (level 1) must appear before bob (level 2).
	paymentsBlock := report[paymentsIdx:]
	aliceIdx := strings.Index(paymentsBlock, "alice")
	bobIdx := strings.Index(paymentsBlock, "bob")
	if aliceIdx == -1 || bobIdx == -1 {
		t.Fatalf("missing user entries in Payments block:\n%s", paymentsBlock)
	}
	if aliceIdx >= bobIdx {
		t.Error("expected alice (level 1) to appear before bob (level 2) within the Payments policy")
	}
}
