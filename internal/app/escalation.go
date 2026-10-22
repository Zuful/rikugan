package app

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Zuful/rikugan/internal/escalation/investigate"
	"github.com/Zuful/rikugan/internal/escalation/opencode"
	"github.com/Zuful/rikugan/internal/watch"
)

// unknownPricePerMTokens prices the tokens of a model whose cost
// OpenCode does not know, for the monthly budget: on the high side, so
// that an unknown cost never slips past it.
const unknownPricePerMTokens = 15.0

// serveEscalation reads the escalation's configuration: nil unless
// RIKUGAN_ESCALATION=on. It needs the agent MCP server (the agent's
// tools), a notification channel (the thread it answers in) and the
// OpenCode server's URL and password.
func serveEscalation(agent *AgentMCPConfig, notifier *watch.Notifier) (*watch.Escalation, error) {
	switch v := os.Getenv("RIKUGAN_ESCALATION"); v {
	case "", "off":
		return nil, nil
	case "on":
	default:
		return nil, fmt.Errorf("RIKUGAN_ESCALATION=%q: want on or off", v)
	}
	switch {
	case agent == nil:
		return nil, errors.New("RIKUGAN_ESCALATION=on needs the agent MCP server: set RIKUGAN_AGENT_MCP_LISTEN and RIKUGAN_AGENT_MCP_TOKEN")
	case notifier == nil || len(notifier.Channels) == 0:
		return nil, errors.New("RIKUGAN_ESCALATION=on needs a notification channel: investigations answer the critical's thread")
	}
	client, err := opencode.New(os.Getenv("OPENCODE_URL"), os.Getenv("OPENCODE_SERVER_PASSWORD"))
	if err != nil {
		return nil, fmt.Errorf("RIKUGAN_ESCALATION=on: %w (OPENCODE_URL, OPENCODE_SERVER_PASSWORD)", err)
	}
	inv := &investigate.Investigator{Runner: client, Teams: splitList(os.Getenv("RIKUGAN_ESCALATION_TEAMS"))}
	e := &watch.Escalation{Investigator: inv, UnknownPricePerMTokens: unknownPricePerMTokens, StaleAfter: 30 * time.Minute}
	for _, v := range []struct {
		name string
		def  int
		dst  *int
	}{
		{"RIKUGAN_ESCALATION_PER_HOUR", 4, &e.PerHour},
		{"RIKUGAN_ESCALATION_CONCURRENCY", 1, &e.Concurrency},
		{"RIKUGAN_ESCALATION_MAX_CALLS", 40, &inv.MaxCalls},
		{"RIKUGAN_ESCALATION_MAX_TOKENS", 200000, &inv.MaxTokens},
	} {
		*v.dst = v.def
		if raw := os.Getenv(v.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("%s: want a positive integer, got %q", v.name, raw)
			}
			*v.dst = n
		}
	}
	e.MonthlyBudget = 5
	if raw := os.Getenv("RIKUGAN_ESCALATION_MONTHLY_BUDGET"); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("RIKUGAN_ESCALATION_MONTHLY_BUDGET: want a positive amount in dollars, got %q", raw)
		}
		e.MonthlyBudget = f
	}
	inv.Timeout = 10 * time.Minute
	if raw := os.Getenv("RIKUGAN_ESCALATION_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Minute {
			return nil, fmt.Errorf("RIKUGAN_ESCALATION_TIMEOUT: want a duration of at least 1m, got %q", raw)
		}
		inv.Timeout = d
	}
	return e, nil
}
