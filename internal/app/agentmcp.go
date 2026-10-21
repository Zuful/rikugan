package app

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"k8s.io/client-go/kubernetes"

	"github.com/Zuful/rikugan/pkg/dispatcher"
	pkgk8s "github.com/Zuful/rikugan/pkg/kubernetes"
	pkgmcp "github.com/Zuful/rikugan/pkg/mcp"
)

// The agent MCP server is what rikugan serve exposes to the agent that
// investigates a critical: the unified binary's tools, read-only
// whatever RIKUGAN_READ_ONLY says, over Streamable HTTP, behind a token
// of its own. The agent holds no backend credential: rikugan calls the
// backends for it.

// AgentMCPPath is the agent MCP server's endpoint.
const AgentMCPPath = "/mcp"

// Output bounds, applied before a tool's output reaches the agent's
// model: they protect its context, the cost, and how much text an
// injection can carry.
const (
	agentMaxBytes    = 32 << 10
	agentMaxLogLines = 200
	minAgentToken    = 32
)

// logTools return log lines, which may carry personal data:
// RIKUGAN_ESCALATION_LOGS=off keeps them from the agent.
var logTools = []string{"get_logs", "search_logs", "tail_logs", "detect_log_anomalies"}

// AgentMCPConfig is the agent MCP server's configuration.
type AgentMCPConfig struct {
	Listen string
	Token  string
	// Logs exposes the tools that return log lines.
	Logs bool
}

// agentMCPConfig reads the agent MCP server's configuration; nil when
// RIKUGAN_AGENT_MCP_LISTEN is unset, the server then off. The token is
// required whatever the address, loopback included: unlike the verdicts
// API, this port reaches every backend.
func agentMCPConfig(serveToken string) (*AgentMCPConfig, error) {
	listen := os.Getenv("RIKUGAN_AGENT_MCP_LISTEN")
	if listen == "" {
		return nil, nil
	}
	cfg := &AgentMCPConfig{Listen: listen, Token: os.Getenv("RIKUGAN_AGENT_MCP_TOKEN")}
	switch {
	case cfg.Token == "":
		return nil, fmt.Errorf("RIKUGAN_AGENT_MCP_LISTEN is set without RIKUGAN_AGENT_MCP_TOKEN: the agent MCP server always requires a token, loopback included")
	case len(cfg.Token) < minAgentToken:
		return nil, fmt.Errorf("RIKUGAN_AGENT_MCP_TOKEN is %d characters: use at least %d (openssl rand -hex 32)", len(cfg.Token), minAgentToken)
	case serveToken != "" && cfg.Token == serveToken:
		return nil, fmt.Errorf("RIKUGAN_AGENT_MCP_TOKEN equals RIKUGAN_SERVE_TOKEN: give the agent a token of its own")
	}
	switch v := os.Getenv("RIKUGAN_ESCALATION_LOGS"); v {
	case "", "on":
		cfg.Logs = true
	case "off":
	default:
		return nil, fmt.Errorf("RIKUGAN_ESCALATION_LOGS=%q: want on or off", v)
	}
	return cfg, nil
}

// SetupAgent builds the agent MCP server's tools: those of the unified
// binary, read-only, outputs bounded, the log tools left out unless
// cfg.Logs. Unlike Setup, Kubernetes is only required when it is the
// orchestration backend.
func SetupAgent(cfg AgentMCPConfig, logger *slog.Logger, version string) (*server.MCPServer, error) {
	var k8sClient kubernetes.Interface
	if os.Getenv("ORCHESTRATION_PROVIDER") != "dockercompose" {
		c, err := pkgk8s.BuildClient("")
		if err != nil {
			return nil, fmt.Errorf("build k8s client: %w", err)
		}
		k8sClient = c
	}
	return setupAgent(cfg, k8sClient, logger, version)
}

func setupAgent(cfg AgentMCPConfig, k8sClient kubernetes.Interface, logger *slog.Logger, version string) (*server.MCPServer, error) {
	logger = logger.With(slog.String("mcp", "agent"))
	srv := pkgmcp.NewServer("rikugan-agent", version,
		server.WithToolHandlerMiddleware(boundOutputs(agentMaxBytes, agentMaxLogLines)))
	opts := []dispatcher.Option{dispatcher.WithReadOnly(true)}
	if !cfg.Logs {
		opts = append(opts, dispatcher.WithoutTools(logTools...))
	}
	if err := registerAll(dispatcher.New(srv, logger, opts...), k8sClient, logger); err != nil {
		return nil, err
	}
	return srv, nil
}

// AgentHandler serves srv over Streamable HTTP at AgentMCPPath, every
// request requiring "Authorization: Bearer <token>", compared in
// constant time.
func AgentHandler(srv *server.MCPServer, token string) http.Handler {
	mcpHTTP := server.NewStreamableHTTPServer(srv, server.WithEndpointPath(AgentMCPPath))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="rikugan-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHTTP.ServeHTTP(w, r)
	})
}

// boundOutputs caps each tool's text output: log tools to maxLogLines
// lines, every tool to maxBytes, saying what was cut.
func boundOutputs(maxBytes, maxLogLines int) server.ToolHandlerMiddleware {
	isLog := map[string]bool{}
	for _, t := range logTools {
		isLog[t] = true
	}
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := next(ctx, req)
			if err != nil || res == nil {
				return res, err
			}
			budget := maxBytes
			for i, c := range res.Content {
				text, ok := c.(mcp.TextContent)
				if !ok {
					continue
				}
				if isLog[req.Params.Name] {
					text.Text = capLines(text.Text, maxLogLines)
				}
				text.Text = capBytes(text.Text, budget)
				budget = max(0, budget-len(text.Text))
				res.Content[i] = text
			}
			return res, nil
		}
	}
}

func capLines(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "") + fmt.Sprintf("\n[truncated by rikugan: %d of %d lines]\n", n, len(lines))
}

func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[truncated by rikugan: %d of %d bytes]\n", cut, len(s))
}
