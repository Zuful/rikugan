// Package reports exposes get_watch_reports: what has rikugan serve
// found lately? It reads the daemon's verdicts API, so an investigating
// agent sees the proactive mode's verdicts without access to the
// daemon's state directory.
package reports

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/pkg/provider"
	"github.com/Zuful/rikugan/pkg/restclient"
)

const opTimeout = 30 * time.Second

// Provider registers get_watch_reports.
type Provider struct {
	baseURL string
	rest    *restclient.Client
	logger  *slog.Logger
}

// New returns a Provider reading the verdicts API at baseURL, with the
// bearer token when the daemon requires one.
func New(baseURL, token string, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	opts := []restclient.Option{
		restclient.WithStatusHint(http.StatusUnauthorized,
			"rikugan serve refused the token (HTTP 401) — check RIKUGAN_SERVE_TOKEN"),
	}
	if token != "" {
		opts = append(opts, restclient.WithHeader("Authorization", "Bearer "+token))
	}
	return &Provider{baseURL: baseURL, rest: restclient.New("rikugan serve", opts...), logger: logger}
}

// FromEnv builds a Provider from RIKUGAN_SERVE_URL and, optionally,
// RIKUGAN_SERVE_TOKEN.
func FromEnv(logger *slog.Logger) (*Provider, error) {
	base := os.Getenv("RIKUGAN_SERVE_URL")
	if base == "" {
		return nil, errors.New("RIKUGAN_SERVE_URL is not set (the address of a running rikugan serve, e.g. http://127.0.0.1:8080)")
	}
	return New(base, os.Getenv("RIKUGAN_SERVE_TOKEN"), logger), nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "watch/reports" }

// Tools implements provider.Provider.
func (p *Provider) Tools() []provider.ToolDefinition {
	tool := mcp.NewTool("get_watch_reports",
		mcp.WithDescription(
			"What has the proactive mode (rikugan serve) found lately? Lists the verdicts it recorded "+
				"after each rollout (+5, +15 and +30 min checkpoints, and failed rollouts) and in its "+
				"periodic sweep (every service, every 4 hours by default), newest first: "+
				"ok / investigate / degraded / insufficient_data, with the signals that were not ok. "+
				"Start here when asked whether a recent deploy went well, then use check_deploy_health "+
				"or the LEST tools to dig into a verdict. "+
				"[Methodology: Step 0 — what changed? — see docs/methodology.md]",
		),
		mcp.WithString("service", mcp.Description("Service as namespace/name, or a bare name for every namespace. Empty for all services.")),
		mcp.WithString("since", mcp.Description("How far back: a duration such as 6h, or an RFC 3339 time. Defaults to 24h.")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of verdicts (default 50).")),
	)
	return []provider.ToolDefinition{{Tool: tool, Handler: p.handle}}
}

func (p *Provider) handle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	params := url.Values{}
	if s := req.GetString("service", ""); s != "" {
		params.Set("service", s)
	}
	since := req.GetString("since", "24h")
	params.Set("since", since)
	params.Set("limit", strconv.Itoa(req.GetInt("limit", 50)))

	var resp watch.VerdictsResponse
	if err := p.rest.GetJSON(ctx, restclient.JoinURL(p.baseURL, "/v1/verdicts", params), &resp); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(Render(resp.Verdicts, req.GetString("service", ""), since)), nil
}

// Render formats verdicts as Markdown for the calling agent.
func Render(verdicts []watch.Verdict, service, since string) string {
	var b strings.Builder
	scope := "all services"
	if service != "" {
		scope = "`" + service + "`"
	}
	fmt.Fprintf(&b, "## Proactive mode verdicts: %s, since %s\n\n", scope, since)
	if len(verdicts) == 0 {
		b.WriteString("No verdict recorded. Either no rollout completed in that period, or rikugan serve does not watch this service.\n")
		return b.String()
	}
	b.WriteString("| Time (UTC) | Service | Revision | Check | Verdict | Signals not ok |\n|---|---|---|---|---|---|\n")
	for _, v := range verdicts {
		cp := "+" + v.Checkpoint
		switch {
		case v.Source == "sweep":
			cp = "periodic sweep"
		case v.Checkpoint == "0s":
			cp = "rollout failure"
		}
		if v.Late {
			cp += " (late)"
		}
		verdict := v.Verdict
		if verdict == "" {
			verdict = "— (" + v.Outcome + ")"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			v.Time.UTC().Format("2006-01-02 15:04"), v.Service, v.Revision, cp, verdict, cell(notOK(v.Signals)))
	}
	return b.String()
}

func notOK(signals []watch.Signal) string {
	var out []string
	for _, s := range signals {
		if s.Status == "investigate" || s.Status == "degraded" {
			out = append(out, fmt.Sprintf("%s %s: %s", s.Name, s.Status, s.Reason))
		}
	}
	return strings.Join(out, "; ")
}

func cell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}
