// Package telegram is the Telegram backend of the communication concept:
// it posts team messages to one chat through the Bot API.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Zuful/rikugan/pkg/provider"
	"github.com/Zuful/rikugan/pkg/restclient"
)

// Compile-time check that *Provider satisfies provider.CommunicationProvider.
var _ provider.CommunicationProvider = (*Provider)(nil)

// baseURL is the Bot API root. It's a var rather than a const so tests can
// swap it for an httptest.Server URL; production code never overrides it.
var baseURL = "https://api.telegram.org"

// Telegram caps a message at 4096 characters after entity parsing. The
// title and body are cut well below that so the severity line and the
// links always fit.
const (
	maxTitle = 256
	maxBody  = 3000
)

// Provider posts messages to one Telegram chat.
type Provider struct {
	token  string
	chatID string
	rest   *restclient.Client
	logger *slog.Logger
}

// New returns a Provider that posts to chatID with the bot token. The
// token sits in the request path, so the client never echoes the URL in
// its errors.
func New(token, chatID string, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{
		token:  token,
		chatID: chatID,
		rest: restclient.New("telegram",
			restclient.WithSecretURL(),
			restclient.WithStatusHint(http.StatusUnauthorized,
				"telegram rejected the bot token (HTTP 401) — check TELEGRAM_BOT_TOKEN"),
			restclient.WithStatusHint(http.StatusNotFound,
				"telegram does not know this bot (HTTP 404) — check TELEGRAM_BOT_TOKEN"),
		),
		logger: logger.With(slog.String("provider", "communication/telegram")),
	}
}

// FromEnv builds a Provider from TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID.
// Both are required.
func FromEnv(logger *slog.Logger) (*Provider, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	switch {
	case token == "":
		return nil, errors.New("telegram: TELEGRAM_BOT_TOKEN is not set (create a bot with @BotFather)")
	case chatID == "":
		return nil, errors.New("telegram: TELEGRAM_CHAT_ID is not set (the chat or channel the bot posts to)")
	}
	return New(token, chatID, logger), nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "communication/telegram" }

// Tools implements provider.Provider. Messages leave from the daemon, not
// from an agent, so the backend exposes no MCP tool.
func (p *Provider) Tools() []provider.ToolDefinition { return nil }

type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	// ReplyParameters threads a reply under an earlier message.
	ReplyParameters *replyParameters `json:"reply_parameters,omitempty"`
}

type replyParameters struct {
	MessageID int64 `json:"message_id"`
	// AllowSendingWithoutReply still posts the reply when the earlier
	// message was deleted.
	AllowSendingWithoutReply bool `json:"allow_sending_without_reply"`
}

type sendMessageResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

// PostMessage implements provider.CommunicationProvider.
func (p *Provider) PostMessage(ctx context.Context, msg provider.TeamMessage) (provider.MessageRef, error) {
	req := sendMessageRequest{
		ChatID:                p.chatID,
		Text:                  Render(msg),
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
	}
	if msg.ReplyTo != nil && msg.ReplyTo.Ref != "" {
		id, err := strconv.ParseInt(string(msg.ReplyTo.Ref), 10, 64)
		if err != nil {
			return "", fmt.Errorf("telegram: %q is not a message id", msg.ReplyTo.Ref)
		}
		req.ReplyParameters = &replyParameters{MessageID: id, AllowSendingWithoutReply: true}
	}
	var resp sendMessageResponse
	endpoint := baseURL + "/bot" + url.PathEscape(p.token) + "/sendMessage"
	if err := p.rest.PostJSON(ctx, endpoint, req, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("telegram refused the message: %s", resp.Description)
	}
	p.logger.Debug("message posted", slog.String("severity", string(msg.Severity)))
	var ref provider.MessageRef
	if resp.Result.MessageID != 0 {
		ref = provider.MessageRef(strconv.FormatInt(resp.Result.MessageID, 10))
	}
	return ref, nil
}

func Render(msg provider.TeamMessage) string {
	var b strings.Builder
	if msg.ReplyTo != nil && msg.ReplyTo.Ref == "" {
		b.WriteString("↪ Re: ")
		b.WriteString(html.EscapeString(truncate(msg.ReplyTo.Title, maxTitle)))
		b.WriteString("\n")
	}
	b.WriteString(marker(msg.Severity))
	b.WriteString(" <b>")
	b.WriteString(html.EscapeString(truncate(msg.Title, maxTitle)))
	b.WriteString("</b>")
	if body := strings.TrimSpace(msg.Body); body != "" {
		b.WriteString("\n\n")
		b.WriteString(html.EscapeString(truncate(body, maxBody)))
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
			fmt.Fprintf(&b, `<a href="%s">%s</a>`, html.EscapeString(l.URL), html.EscapeString(text))
		} else {
			b.WriteString(html.EscapeString(text + ": " + l.URL))
		}
	}
	return b.String()
}

func marker(s provider.Severity) string {
	switch s {
	case provider.SeverityCritical:
		return "🔴"
	case provider.SeverityWarning:
		return "🟠"
	default:
		return "🔵"
	}
}

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
