package reports

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/watch"
)

func call(t *testing.T, p *Provider, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_watch_reports"
	req.Params.Arguments = args
	res, err := p.Tools()[0].Handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return strings.Join(out, "\n")
}

func TestGetWatchReports(t *testing.T) {
	var query, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, auth = r.URL.RawQuery, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(watch.VerdictsResponse{Verdicts: []watch.Verdict{
			{Time: time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC), Service: "shop/checkout", Revision: "8",
				Checkpoint: "15m0s", Partial: true, Outcome: "evaluated", Verdict: "degraded",
				Signals: []watch.Signal{
					{Name: "error_rate", Status: "degraded", Reason: "5xx 0.20% → 6.00% | spike"},
					{Name: "latency", Status: "ok", Reason: "fine"},
				}},
			{Time: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), Service: "shop/checkout", Revision: "7",
				Checkpoint: "0s", Late: true, Outcome: "evaluated", Verdict: "degraded"},
		}})
	}))
	defer srv.Close()

	res := call(t, New(srv.URL, "s3cret", nil), map[string]any{"service": "shop/checkout", "since": "6h"})
	if res.IsError {
		t.Fatalf("error: %s", text(res))
	}
	if auth != "Bearer s3cret" {
		t.Errorf("authorization %q", auth)
	}
	if query != "limit=50&service=shop%2Fcheckout&since=6h" {
		t.Errorf("query %q", query)
	}
	out := text(res)
	for _, want := range []string{
		"## Proactive mode verdicts: `shop/checkout`, since 6h",
		"| 2026-10-19 09:30 | shop/checkout | 8 | +15m0s (metrics down) | degraded | error_rate degraded: 5xx 0.20% → 6.00% \\| spike |",
		"| 2026-10-19 09:00 | shop/checkout | 7 | rollout failure (late) | degraded | — |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "latency") {
		t.Errorf("ok signals listed:\n%s", out)
	}
}

func TestGetWatchReportsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	res := call(t, New(srv.URL, "", nil), nil)
	if !res.IsError || !strings.Contains(text(res), "RIKUGAN_SERVE_TOKEN") {
		t.Errorf("401: %s", text(res))
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"verdicts":[]}`))
	}))
	defer empty.Close()
	if out := text(call(t, New(empty.URL, "", nil), nil)); !strings.Contains(out, "No verdict recorded") || !strings.Contains(out, "all services, since 24h") {
		t.Errorf("empty: %s", out)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("RIKUGAN_SERVE_URL", "")
	if _, err := FromEnv(nil); err == nil {
		t.Error("missing URL accepted")
	}
	t.Setenv("RIKUGAN_SERVE_URL", "http://127.0.0.1:8080")
	if p, err := FromEnv(nil); err != nil || p.Name() != "watch/reports" {
		t.Errorf("%v %v", p, err)
	}
}
