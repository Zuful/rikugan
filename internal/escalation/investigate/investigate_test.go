package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/escalation"
	"github.com/Zuful/rikugan/internal/escalation/opencode"
	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/pkg/provider"
)

type fakeRunner struct {
	natives []string
	preErr  error
	res     opencode.Result
	runErr  error
	req     opencode.Request
}

func (f *fakeRunner) Preflight(context.Context) ([]string, error) { return f.natives, f.preErr }
func (f *fakeRunner) Run(_ context.Context, req opencode.Request) (opencode.Result, error) {
	f.req = req
	return f.res, f.runErr
}

var inc = watch.Incident{
	Key: "rollout/shop/api/7", Title: "shop/api: revision 7 degraded (+5m0s)", Body: "- error_rate: degraded, 30% (was 0.1%)",
	At:        time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC),
	Services:  []provider.Service{{Namespace: "shop", Name: "api"}},
	Revisions: map[string][]string{"shop/api": {"7", "6"}},
	Signals:   []string{"error_rate"},
}

func answer(t *testing.T, f escalation.Finding) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInvestigateDone(t *testing.T) {
	r := &fakeRunner{natives: []string{"bash", "read"}, res: opencode.Result{
		SessionID: "ses_1", Tokens: 12000, Cost: 0.04,
		Calls: []escalation.ToolCall{{Tool: "get_logs", Output: "panic: nil map write"}, {Tool: "get_recent_commits", Output: "abc refactor cart"}},
		Answer: answer(t, escalation.Finding{
			Cause:      "A nil map write since revision 7.",
			Evidence:   []escalation.Evidence{{Tool: "get_logs", Quote: "panic: nil map write"}},
			Action:     escalation.Action{Kind: escalation.ActionRollback, Service: "shop/api", Revision: "6"},
			Confidence: escalation.ConfidenceHigh,
		}),
	}}
	inv := (&Investigator{Runner: r, Timeout: time.Minute}).Investigate(context.Background(), inc)
	if inv.Outcome != watch.EscalationDone || inv.Session != "ses_1" || inv.Calls != 2 || inv.Tokens != 12000 {
		t.Errorf("investigation %+v", inv)
	}
	for _, want := range []string{"Suggested action: roll shop/api back to revision 6", "Confidence: high", escalation.Disclaimer} {
		if !strings.Contains(inv.Reply.Body, want) {
			t.Errorf("reply lacks %q:\n%s", want, inv.Reply.Body)
		}
	}
	if r.req.System != System || strings.Join(r.req.Natives, ",") != "bash,read" || r.req.Schema == nil {
		t.Errorf("request %+v", r.req)
	}
	for _, want := range []string{"revision 7 rolled out; earlier: 6", "error_rate: degraded", "shop/api: revision 7 degraded"} {
		if !strings.Contains(r.req.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, r.req.Prompt)
		}
	}
}

// The agent cites tools under OpenCode's prefix: its quotes are still
// checked against the calls.
func TestInvestigateUnprefixesTools(t *testing.T) {
	r := &fakeRunner{res: opencode.Result{
		Calls: []escalation.ToolCall{{Tool: "get_logs", Output: "panic: nil map write"}},
		Answer: answer(t, escalation.Finding{
			Cause:      "A nil map write.",
			Evidence:   []escalation.Evidence{{Tool: "rikugan_get_logs", Quote: "panic: nil map write"}},
			RuledOut:   []escalation.RuledOut{{Hypothesis: "a bad config", Reason: "unchanged", Tool: "rikugan_get_logs", Quote: "nil map"}},
			Action:     escalation.Action{Kind: escalation.ActionInspect, Target: "rikugan_get_logs"},
			Confidence: escalation.ConfidenceMedium,
		}),
	}}
	inv := (&Investigator{Runner: r, Timeout: time.Minute}).Investigate(context.Background(), inc)
	if strings.Contains(inv.Reply.Body, escalation.Unverified) || strings.Contains(inv.Reply.Body, "rikugan_") {
		t.Errorf("prefixed tools not matched with the calls:\n%s", inv.Reply.Body)
	}
}

func TestInvestigateFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       *fakeRunner
		outcome string
		says    string
	}{
		{"preflight blocks", &fakeRunner{preErr: errors.New("native tools unknown to this version of rikugan: browser")},
			watch.EscalationBlocked, "Not investigated: OpenCode server check failed: native tools unknown"},
		{"native tool called", &fakeRunner{res: opencode.Result{Native: []string{"bash", "read", "bash"}, Answer: json.RawMessage(`{}`)}},
			watch.EscalationFailed, "tools rikugan forbids (bash, read)"},
		{"stopped without an answer", &fakeRunner{res: opencode.Result{Stopped: "timeout"}},
			watch.EscalationTimeout, "stopped without an answer (time limit reached)"},
		{"model error", &fakeRunner{res: opencode.Result{Err: "ProviderAuthError: invalid x-api-key"}},
			watch.EscalationFailed, "Investigation failed: ProviderAuthError"},
		{"no structured answer", &fakeRunner{res: opencode.Result{}},
			watch.EscalationFailed, "no answer in the required shape"},
		{"server error", &fakeRunner{runErr: errors.New("create session: opencode returned 500")},
			watch.EscalationFailed, "Investigation failed: create session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := (&Investigator{Runner: tc.r, Timeout: time.Minute}).Investigate(context.Background(), inc)
			if inv.Outcome != tc.outcome || !strings.Contains(inv.Reply.Body, tc.says) {
				t.Errorf("outcome %s, reply %q; want %s, %q", inv.Outcome, inv.Reply.Body, tc.outcome, tc.says)
			}
			if tc.outcome == watch.EscalationBlocked && inv.Reason == "" {
				t.Error("blocked without a reason")
			}
		})
	}
}

func TestPrompt(t *testing.T) {
	grouped := inc
	grouped.Grouped, grouped.Sweep, grouped.MetricsDown = true, true, true
	p := Prompt(grouped)
	for _, want := range []string{"common cause", "metrics backend is unavailable"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}
