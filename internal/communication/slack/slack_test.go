package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zuful/rikugan/pkg/provider"
	"github.com/Zuful/rikugan/pkg/restclient"
)

// newTestProvider points a Provider at srv. httptest.NewTLSServer gives an
// https URL, which New requires, and a client that trusts it.
func newTestProvider(t *testing.T, srv *httptest.Server, path string) *Provider {
	t.Helper()
	p, err := New(srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.rest = newRest(restclient.WithHTTPClient(srv.Client()))
	return p
}

func TestPostMessage_SendsMrkdwnText(t *testing.T) {
	var path string
	var got map[string]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv, "/services/T000/B000/XXXX")
	_, err := p.PostMessage(context.Background(), provider.TeamMessage{
		Severity: provider.SeverityCritical,
		Title:    "checkout: degraded",
		Body:     "5xx rate 6% (was 0.2%)",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/services/T000/B000/XXXX" {
		t.Errorf("path %q", path)
	}
	if want := ":red_circle: *checkout: degraded*\n\n5xx rate 6% (was 0.2%)"; got["text"] != want {
		t.Errorf("text %q, want %q", got["text"], want)
	}
}

func TestPostMessage_RevokedWebhookHint(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no_service"))
	}))
	defer srv.Close()

	_, err := newTestProvider(t, srv, "/services/SECRET").PostMessage(context.Background(), provider.TeamMessage{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "SLACK_WEBHOOK_URL") {
		t.Fatalf("got %v, want the webhook hint", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the webhook: %v", err)
	}
}

func TestRender_EscapesAndFormatsLinks(t *testing.T) {
	got := Render(provider.TeamMessage{
		Severity: provider.SeverityWarning,
		Title:    "api <v2> & co",
		Body:     "p95 > 300ms",
		Links: []provider.Link{
			{Text: "Grafana", URL: "https://grafana.example/d?a=1&b=2|x"},
			{Text: "script", URL: "javascript:alert(1)"},
		},
	})
	want := ":large_orange_circle: *api &lt;v2&gt; &amp; co*\n\np95 &gt; 300ms\n\n" +
		"<https://grafana.example/d?a=1&b=2%7Cx|Grafana>\n" +
		"script: javascript:alert(1)"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestNew_RequiresHTTPS(t *testing.T) {
	for _, raw := range []string{"", "http://hooks.slack.com/services/x", "hooks.slack.com/services/x"} {
		if _, err := New(raw, nil); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("SLACK_WEBHOOK_URL", "")
	if _, err := FromEnv(nil); err == nil || !strings.Contains(err.Error(), "SLACK_WEBHOOK_URL") {
		t.Errorf("missing URL: %v", err)
	}
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/T/B/X")
	p, err := FromEnv(nil)
	if err != nil || p.Name() != "communication/slack" || p.Tools() != nil {
		t.Errorf("configured: %v, %v", p, err)
	}
}

func TestBot_PostsReturnsTheTSAndThreads(t *testing.T) {
	var auth string
	var got postMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		got = postMessageRequest{}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1729.0001"}`))
	}))
	defer srv.Close()
	apiURL = srv.URL
	p, err := NewBot("xoxb-1-2", "C0123", nil)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := p.PostMessage(context.Background(), provider.TeamMessage{Severity: provider.SeverityCritical, Title: "critical"})
	if err != nil || ref != "1729.0001" {
		t.Fatalf("ref %q, err %v", ref, err)
	}
	if auth != "Bearer xoxb-1-2" || got.Channel != "C0123" || got.ThreadTS != "" || got.UnfurlLinks {
		t.Errorf("auth %q, request %+v", auth, got)
	}
	if _, err := p.PostMessage(context.Background(), provider.TeamMessage{
		Title: "Investigation", ReplyTo: &provider.Reply{Ref: ref, Title: "critical"},
	}); err != nil {
		t.Fatal(err)
	}
	if got.ThreadTS != "1729.0001" || strings.Contains(got.Text, "Re:") {
		t.Errorf("reply request %+v", got)
	}
}

func TestBot_SlackRefusalWithHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
	}))
	defer srv.Close()
	apiURL = srv.URL
	p, _ := NewBot("xoxb-SECRET", "C0123", nil)
	_, err := p.PostMessage(context.Background(), provider.TeamMessage{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "not_in_channel") || !strings.Contains(err.Error(), "invite the bot") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the token: %v", err)
	}
}

// A webhook cannot thread: a reply quotes the message it answers, even
// given a reference.
func TestWebhook_ReplyQuotes(t *testing.T) {
	var got map[string]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	ref, err := newTestProvider(t, srv, "/services/X").PostMessage(context.Background(), provider.TeamMessage{
		Title: "Investigation", ReplyTo: &provider.Reply{Ref: "1729.0001", Title: "api <degraded>"},
	})
	if err != nil || ref != "" {
		t.Fatalf("ref %q, err %v", ref, err)
	}
	if !strings.HasPrefix(got["text"], "↪ Re: api &lt;degraded&gt;\n") {
		t.Errorf("text %q", got["text"])
	}
}

func TestFromEnv_BotOrWebhook(t *testing.T) {
	for _, tc := range []struct {
		token, channel, webhook, fails string
		bot                            bool
	}{
		{webhook: "https://hooks.slack.com/services/X"},
		{token: "xoxb-1", channel: "C1", bot: true},
		{token: "xoxb-1", channel: "C1", webhook: "https://hooks.slack.com/services/X", fails: "not both"},
		{token: "xoxb-1", fails: "SLACK_CHANNEL"},
		{token: "xoxp-user", channel: "C1", fails: "bot token"},
	} {
		t.Setenv("SLACK_BOT_TOKEN", tc.token)
		t.Setenv("SLACK_CHANNEL", tc.channel)
		t.Setenv("SLACK_WEBHOOK_URL", tc.webhook)
		p, err := FromEnv(nil)
		if tc.fails != "" {
			if err == nil || !strings.Contains(err.Error(), tc.fails) {
				t.Errorf("%+v: err %v, want %q", tc, err, tc.fails)
			}
			continue
		}
		if err != nil || (p.botToken != "") != tc.bot {
			t.Errorf("%+v: provider %+v, err %v", tc, p, err)
		}
	}
}
