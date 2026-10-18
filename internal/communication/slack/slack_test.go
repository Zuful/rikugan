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
	err := p.PostMessage(context.Background(), provider.TeamMessage{
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

	err := newTestProvider(t, srv, "/services/SECRET").PostMessage(context.Background(), provider.TeamMessage{Title: "t"})
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
