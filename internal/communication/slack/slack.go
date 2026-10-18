// Package slack is the Slack backend of the communication concept: it
// posts team messages through an incoming webhook, which is bound to one
// channel when the Slack app is installed.
package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Zuful/rikugan/pkg/provider"
	"github.com/Zuful/rikugan/pkg/restclient"
)

// Compile-time check that *Provider satisfies provider.CommunicationProvider.
var _ provider.CommunicationProvider = (*Provider)(nil)

// Slack truncates long messages in the channel view. The title and body
// are cut so a message stays readable without expanding it.
const (
	maxTitle = 256
	maxBody  = 3000
)

// Provider posts messages to one Slack incoming webhook.
type Provider struct {
	webhookURL string
	rest       *restclient.Client
	logger     *slog.Logger
}

// New returns a Provider for the incoming webhook at webhookURL, which
// must be https. The URL is the credential, so the client never echoes it
// in its errors.
func New(webhookURL string, logger *slog.Logger) (*Provider, error) {
	u, err := url.Parse(webhookURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("slack: the webhook URL must be an https URL (https://hooks.slack.com/services/…)")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		webhookURL: webhookURL,
		rest:       newRest(),
		logger:     logger.With(slog.String("provider", "communication/slack")),
	}, nil
}

// newRest returns the webhook client: secret URL, and a hint for each
// status that means the webhook itself is no longer valid.
func newRest(opts ...restclient.Option) *restclient.Client {
	opts = append([]restclient.Option{
		restclient.WithSecretURL(),
		restclient.WithStatusHint(http.StatusForbidden,
			"slack refused the webhook (HTTP 403) — check SLACK_WEBHOOK_URL"),
		restclient.WithStatusHint(http.StatusNotFound,
			"slack does not know this webhook (HTTP 404), it may have been revoked — check SLACK_WEBHOOK_URL"),
		restclient.WithStatusHint(http.StatusGone,
			"the slack channel behind this webhook was archived or deleted (HTTP 410) — check SLACK_WEBHOOK_URL"),
	}, opts...)
	return restclient.New("slack", opts...)
}

// FromEnv builds a Provider from SLACK_WEBHOOK_URL.
func FromEnv(logger *slog.Logger) (*Provider, error) {
	raw := os.Getenv("SLACK_WEBHOOK_URL")
	if raw == "" {
		return nil, errors.New("slack: SLACK_WEBHOOK_URL is not set (create an incoming webhook in your Slack app)")
	}
	return New(raw, logger)
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "communication/slack" }

// Tools implements provider.Provider. Messages leave from the daemon, not
// from an agent, so the backend exposes no MCP tool.
func (p *Provider) Tools() []provider.ToolDefinition { return nil }

// PostMessage implements provider.CommunicationProvider. Slack answers a
// webhook with plain text ("ok"), so there is nothing to decode.
func (p *Provider) PostMessage(ctx context.Context, msg provider.TeamMessage) error {
	body := map[string]string{"text": Render(msg)}
	if err := p.rest.PostJSON(ctx, p.webhookURL, body, nil); err != nil {
		return err
	}
	p.logger.Debug("message posted", slog.String("severity", string(msg.Severity)))
	return nil
}

// Render formats msg as Slack mrkdwn: a severity marker and the bold
// title, the body, then one line per link. Text is escaped as Slack
// requires (&, <, >), and links that are not http(s) are shown as text.
func Render(msg provider.TeamMessage) string {
	var b strings.Builder
	b.WriteString(marker(msg.Severity))
	b.WriteString(" *")
	b.WriteString(escape(truncate(msg.Title, maxTitle)))
	b.WriteString("*")
	if body := strings.TrimSpace(msg.Body); body != "" {
		b.WriteString("\n\n")
		b.WriteString(escape(truncate(body, maxBody)))
	}
	if len(msg.Links) > 0 {
		b.WriteString("\n")
	}
	for _, l := range msg.Links {
		b.WriteString("\n")
		text := l.Text
		if text == "" {
			text = l.URL
		}
		if isHTTP(l.URL) {
			// In <url|text>, '|' and '>' end the URL: percent-encode them.
			target := strings.NewReplacer("|", "%7C", ">", "%3E", "<", "%3C").Replace(l.URL)
			fmt.Fprintf(&b, "<%s|%s>", target, escape(text))
		} else {
			b.WriteString(escape(text + ": " + l.URL))
		}
	}
	return b.String()
}

func marker(s provider.Severity) string {
	switch s {
	case provider.SeverityCritical:
		return ":red_circle:"
	case provider.SeverityWarning:
		return ":large_orange_circle:"
	default:
		return ":large_blue_circle:"
	}
}

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escape(s string) string { return escaper.Replace(s) }

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// truncate cuts s to at most max runes, marking the cut.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
