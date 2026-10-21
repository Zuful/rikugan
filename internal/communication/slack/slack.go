// Package slack is the Slack backend of the communication concept. It
// posts team messages through an incoming webhook, bound to one channel
// when the Slack app is installed, or through a bot token and
// chat.postMessage. Only the bot learns the posted message's ts, so
// only the bot can answer in a thread; through a webhook, a reply
// quotes the message it answers.
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

// Provider posts messages to one Slack channel, through an incoming
// webhook or a bot.
type Provider struct {
	webhookURL string
	// botToken and channel, instead of webhookURL.
	botToken string
	channel  string
	rest     *restclient.Client
	logger   *slog.Logger
}

// apiURL is Slack's Web API, replaced in tests.
var apiURL = "https://slack.com/api"

// New returns a Provider for the incoming webhook at webhookURL, which
// must be https. The URL is the credential, so the client never echoes it
// in its errors.
func New(webhookURL string, logger *slog.Logger) (*Provider, error) {
	u, err := url.Parse(webhookURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("slack: the webhook URL must be an https URL (https://hooks.slack.com/services/…)")
	}
	return &Provider{webhookURL: webhookURL, rest: newRest(), logger: withLogger(logger)}, nil
}

// NewBot returns a Provider that posts to channel (an ID, C…) with a bot
// token (xoxb-…) holding the chat:write scope.
func NewBot(token, channel string, logger *slog.Logger) (*Provider, error) {
	if !strings.HasPrefix(token, "xoxb-") {
		return nil, errors.New("slack: SLACK_BOT_TOKEN must be a bot token (xoxb-…)")
	}
	if channel == "" {
		return nil, errors.New("slack: SLACK_CHANNEL is not set (the ID of the channel the bot posts to, C…)")
	}
	return &Provider{
		botToken: token,
		channel:  channel,
		rest:     newBotRest(token),
		logger:   withLogger(logger),
	}, nil
}

func withLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(slog.String("provider", "communication/slack"))
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

// newBotRest returns the Web API client, authenticated by the bot token.
func newBotRest(token string, opts ...restclient.Option) *restclient.Client {
	return restclient.New("slack", append([]restclient.Option{
		restclient.WithHeader("Authorization", "Bearer "+token),
	}, opts...)...)
}

// FromEnv builds a Provider from SLACK_BOT_TOKEN and SLACK_CHANNEL, or
// from SLACK_WEBHOOK_URL; setting both is refused, so that which one
// posts is never a guess.
func FromEnv(logger *slog.Logger) (*Provider, error) {
	token, webhook := os.Getenv("SLACK_BOT_TOKEN"), os.Getenv("SLACK_WEBHOOK_URL")
	switch {
	case token != "" && webhook != "":
		return nil, errors.New("slack: set SLACK_BOT_TOKEN or SLACK_WEBHOOK_URL, not both (the bot can answer in threads, the webhook cannot)")
	case token != "":
		return NewBot(token, os.Getenv("SLACK_CHANNEL"), logger)
	case webhook != "":
		return New(webhook, logger)
	}
	return nil, errors.New("slack: neither SLACK_BOT_TOKEN nor SLACK_WEBHOOK_URL is set")
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "communication/slack" }

// Tools implements provider.Provider. Messages leave from the daemon, not
// from an agent, so the backend exposes no MCP tool.
func (p *Provider) Tools() []provider.ToolDefinition { return nil }

type postMessageRequest struct {
	Channel     string `json:"channel"`
	Text        string `json:"text"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	UnfurlLinks bool   `json:"unfurl_links"`
	UnfurlMedia bool   `json:"unfurl_media"`
}

type postMessageResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	TS    string `json:"ts"`
}

// PostMessage implements provider.CommunicationProvider. A webhook
// answers with plain text ("ok") and no reference; the bot returns the
// message's ts, and threads a reply under its Ref.
func (p *Provider) PostMessage(ctx context.Context, msg provider.TeamMessage) (provider.MessageRef, error) {
	if p.botToken == "" {
		if msg.ReplyTo != nil {
			msg.ReplyTo = &provider.Reply{Title: msg.ReplyTo.Title} // no thread through a webhook
		}
		if err := p.rest.PostJSON(ctx, p.webhookURL, map[string]string{"text": Render(msg)}, nil); err != nil {
			return "", err
		}
		p.logger.Debug("message posted", slog.String("severity", string(msg.Severity)))
		return "", nil
	}
	req := postMessageRequest{Channel: p.channel, Text: Render(msg)}
	if msg.ReplyTo != nil {
		req.ThreadTS = string(msg.ReplyTo.Ref)
	}
	var resp postMessageResponse
	if err := p.rest.PostJSON(ctx, apiURL+"/chat.postMessage", req, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("slack refused the message: %s%s", resp.Error, botHint(resp.Error))
	}
	p.logger.Debug("message posted", slog.String("severity", string(msg.Severity)))
	return provider.MessageRef(resp.TS), nil
}

// botHint says what to check for the errors a configuration causes.
func botHint(code string) string {
	switch code {
	case "invalid_auth", "not_authed", "token_revoked", "account_inactive":
		return " — check SLACK_BOT_TOKEN"
	case "channel_not_found", "not_in_channel", "is_archived":
		return " — check SLACK_CHANNEL and invite the bot to the channel"
	case "missing_scope":
		return " — the bot needs the chat:write scope"
	}
	return ""
}

// Render formats msg as Slack mrkdwn: a severity marker and the bold
// title, the body, then one line per link. Text is escaped as Slack
// requires (&, <, >), and links that are not http(s) are shown as text.
func Render(msg provider.TeamMessage) string {
	var b strings.Builder
	if msg.ReplyTo != nil && msg.ReplyTo.Ref == "" {
		b.WriteString("↪ Re: ")
		b.WriteString(escape(truncate(msg.ReplyTo.Title, maxTitle)))
		b.WriteString("\n")
	}
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
