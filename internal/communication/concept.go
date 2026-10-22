// Package communication groups the backends that post messages to a
// team channel: telegram and slack. It is the delivery side of the
// proactive mode. Messages leave from the daemon (rikugan serve), not
// from an investigating agent, so the concept registers no MCP tool.
//
// The concept knows nothing of deploy-health verdicts: it carries a
// provider.TeamMessage with a generic severity, and the daemon maps
// verdicts to severities (health.Status.Severity).
package communication

import (
	"errors"
	"log/slog"
	"os"

	"github.com/Zuful/rikugan/internal/communication/slack"
	"github.com/Zuful/rikugan/internal/communication/telegram"
	"github.com/Zuful/rikugan/pkg/provider"
)

// Configured returns a provider for every channel the environment sets
// up, so every configured channel receives the team's messages. A
// channel is set up as soon as one of its variables is present; a
// half-configured channel is an error rather than a silently missing
// one. No channel at all returns an empty list and no error: whether
// that is acceptable is the caller's decision.
func Configured(logger *slog.Logger) ([]provider.CommunicationProvider, error) {
	var (
		out  []provider.CommunicationProvider
		errs []error
	)
	if os.Getenv("TELEGRAM_BOT_TOKEN") != "" || os.Getenv("TELEGRAM_CHAT_ID") != "" {
		p, err := telegram.FromEnv(logger)
		if err != nil {
			errs = append(errs, err)
		} else {
			out = append(out, p)
		}
	}
	if os.Getenv("SLACK_WEBHOOK_URL") != "" || os.Getenv("SLACK_BOT_TOKEN") != "" || os.Getenv("SLACK_CHANNEL") != "" {
		p, err := slack.FromEnv(logger)
		if err != nil {
			errs = append(errs, err)
		} else {
			out = append(out, p)
		}
	}
	return out, errors.Join(errs...)
}
