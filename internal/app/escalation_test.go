package app

import (
	"strings"
	"testing"

	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/pkg/provider"
)

func TestServeEscalation(t *testing.T) {
	agent := &AgentMCPConfig{Listen: ":8081", Token: agentToken}
	channels := &watch.Notifier{Channels: []provider.CommunicationProvider{&doctorChannel{}}}
	for _, tc := range []struct {
		name     string
		env      map[string]string
		agent    *AgentMCPConfig
		notifier *watch.Notifier
		off      bool
		fails    string
	}{
		{name: "off by default", env: map[string]string{}, off: true},
		{name: "misspelt switch", env: map[string]string{"RIKUGAN_ESCALATION": "true"}, fails: "want on or off"},
		{name: "no agent MCP server", env: map[string]string{"RIKUGAN_ESCALATION": "on"}, notifier: channels, fails: "RIKUGAN_AGENT_MCP_LISTEN"},
		{name: "no channel", env: map[string]string{"RIKUGAN_ESCALATION": "on"}, agent: agent, notifier: &watch.Notifier{}, fails: "notification channel"},
		{name: "no password", env: map[string]string{"RIKUGAN_ESCALATION": "on", "OPENCODE_URL": "http://opencode:4096"}, agent: agent, notifier: channels, fails: "password"},
		{name: "bad budget", env: map[string]string{"RIKUGAN_ESCALATION": "on", "OPENCODE_URL": "http://opencode:4096", "OPENCODE_SERVER_PASSWORD": "pw", "RIKUGAN_ESCALATION_MONTHLY_BUDGET": "0"}, agent: agent, notifier: channels, fails: "MONTHLY_BUDGET"},
		{name: "on", env: map[string]string{"RIKUGAN_ESCALATION": "on", "OPENCODE_URL": "http://opencode:4096", "OPENCODE_SERVER_PASSWORD": "pw", "RIKUGAN_ESCALATION_PER_HOUR": "2"}, agent: agent, notifier: channels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"RIKUGAN_ESCALATION", "OPENCODE_URL", "OPENCODE_SERVER_PASSWORD", "RIKUGAN_ESCALATION_MONTHLY_BUDGET",
				"RIKUGAN_ESCALATION_PER_HOUR", "RIKUGAN_ESCALATION_CONCURRENCY", "RIKUGAN_ESCALATION_MAX_CALLS", "RIKUGAN_ESCALATION_MAX_TOKENS",
				"RIKUGAN_ESCALATION_TIMEOUT", "RIKUGAN_ESCALATION_TEAMS"} {
				t.Setenv(k, tc.env[k])
			}
			e, err := serveEscalation(tc.agent, tc.notifier)
			switch {
			case tc.fails != "":
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatalf("err = %v, want %q", err, tc.fails)
				}
			case err != nil:
				t.Fatal(err)
			case tc.off:
				if e != nil {
					t.Error("on without RIKUGAN_ESCALATION=on")
				}
			case e == nil || e.PerHour != 2 || e.MonthlyBudget != 5 || e.Concurrency != 1:
				t.Errorf("escalation %+v", e)
			}
		})
	}
}
