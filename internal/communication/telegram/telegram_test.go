package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Zuful/rikugan/pkg/provider"
)

func TestPostMessage_SendsHTMLToTheChat(t *testing.T) {
	var path string
	var got sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	baseURL = srv.URL

	p := New("123:ABC", "-100042", slog.Default())
	err := p.PostMessage(context.Background(), provider.TeamMessage{
		Severity: provider.SeverityCritical,
		Title:    "checkout: degraded",
		Body:     "5xx rate 6% (was 0.2%)",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/bot123:ABC/sendMessage" {
		t.Errorf("path %q", path)
	}
	if got.ChatID != "-100042" {
		t.Errorf("chat_id %q", got.ChatID)
	}
	if got.ParseMode != "HTML" || !got.DisableWebPagePreview {
		t.Errorf("parse_mode %q, preview disabled %v", got.ParseMode, got.DisableWebPagePreview)
	}
	if want := "🔴 <b>checkout: degraded</b>\n\n5xx rate 6% (was 0.2%)"; got.Text != want {
		t.Errorf("text %q, want %q", got.Text, want)
	}
}

func TestPostMessage_SurfacesTelegramRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()
	baseURL = srv.URL

	err := New("123:SECRET", "1", nil).PostMessage(context.Background(), provider.TeamMessage{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("got %v, want Telegram's description", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestPostMessage_BadTokenHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	baseURL = srv.URL

	err := New("bad", "1", nil).PostMessage(context.Background(), provider.TeamMessage{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("got %v, want the token hint", err)
	}
}

func TestRender_EscapesAndFormatsLinks(t *testing.T) {
	got := Render(provider.TeamMessage{
		Severity: provider.SeverityWarning,
		Title:    "api <v2> & co",
		Body:     "p95 > 300ms",
		Links: []provider.Link{
			{Text: "Grafana", URL: "https://grafana.example/d?a=1&b=2"},
			{URL: "https://example.com/x"},
			{Text: "script", URL: "javascript:alert(1)"},
		},
	})
	want := "🟠 <b>api &lt;v2&gt; &amp; co</b>\n\np95 &gt; 300ms\n\n" +
		`<a href="https://grafana.example/d?a=1&amp;b=2">Grafana</a>` + "\n" +
		`<a href="https://example.com/x">https://example.com/x</a>` + "\n" +
		"script: javascript:alert(1)"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRender_InfoAndTruncation(t *testing.T) {
	got := Render(provider.TeamMessage{
		Severity: provider.SeverityInfo,
		Title:    "recovered",
		Body:     strings.Repeat("é", maxBody+500),
	})
	if !strings.HasPrefix(got, "🔵 <b>recovered</b>") {
		t.Errorf("prefix: %q", got[:30])
	}
	if n := utf8.RuneCountInString(got); n > 4096 {
		t.Errorf("rendered %d characters, over Telegram's 4096", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("cut not marked")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "1")
	if _, err := FromEnv(nil); err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Errorf("missing token: %v", err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "t")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	if _, err := FromEnv(nil); err == nil || !strings.Contains(err.Error(), "TELEGRAM_CHAT_ID") {
		t.Errorf("missing chat: %v", err)
	}
	t.Setenv("TELEGRAM_CHAT_ID", "1")
	p, err := FromEnv(nil)
	if err != nil || p.Name() != "communication/telegram" || p.Tools() != nil {
		t.Errorf("configured: %v, %v", p, err)
	}
}
