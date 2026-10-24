package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const agentToken = "0123456789abcdef0123456789abcdef"

func TestAgentMCPConfig(t *testing.T) {
	for _, tc := range []struct {
		name, listen, token, logs, serveToken string
		off                                   bool
		logsOn                                bool
		fails                                 string
	}{
		{name: "off when no address", off: true},
		{name: "loopback still needs a token", listen: "127.0.0.1:8081", fails: "always requires a token"},
		{name: "short token", listen: "127.0.0.1:8081", token: "short", fails: "at least 32"},
		{name: "same token as the verdicts API", listen: ":8081", token: agentToken, serveToken: agentToken, fails: "a token of its own"},
		{name: "logs on by default", listen: ":8081", token: agentToken, logsOn: true},
		{name: "logs off", listen: ":8081", token: agentToken, logs: "off"},
		{name: "misspelt logs switch", listen: ":8081", token: agentToken, logs: "false", fails: "want on or off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RIKUGAN_AGENT_MCP_LISTEN", tc.listen)
			t.Setenv("RIKUGAN_AGENT_MCP_TOKEN", tc.token)
			t.Setenv("RIKUGAN_ESCALATION_LOGS", tc.logs)
			cfg, err := agentMCPConfig(tc.serveToken)
			switch {
			case tc.fails != "":
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatalf("err = %v, want it to say %q", err, tc.fails)
				}
			case err != nil:
				t.Fatal(err)
			case tc.off:
				if cfg != nil {
					t.Errorf("cfg = %+v, want nil", cfg)
				}
			case cfg == nil || cfg.Logs != tc.logsOn:
				t.Errorf("cfg = %+v, want logs %v", cfg, tc.logsOn)
			}
		})
	}
}

// agentServer serves the agent MCP server over HTTP, on Docker Compose
// orchestration with Loki and Prometheus configured, so that every
// concept registers.
func agentServer(t *testing.T, logs bool) *httptest.Server {
	t.Helper()
	t.Setenv("ORCHESTRATION_PROVIDER", "dockercompose")
	t.Setenv("RIKUGAN_READ_ONLY", "false") // the agent server is read-only regardless
	t.Setenv("LOKI_URL", "http://127.0.0.1:1")
	t.Setenv("PROMETHEUS_URL", "http://127.0.0.1:1")
	srv, err := setupAgent(AgentMCPConfig{Token: agentToken, Logs: logs}, nil, discard(), "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(AgentHandler(srv, agentToken))
	t.Cleanup(ts.Close)
	return ts
}

func agentTools(t *testing.T, url, token string) ([]string, error) {
	t.Helper()
	c, err := client.NewStreamableHttpClient(url+AgentMCPPath,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	if _, err := c.Initialize(ctx, init); err != nil {
		return nil, err
	}
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

func TestAgentMCPServesReadToolsOnly(t *testing.T) {
	ts := agentServer(t, true)
	names, err := agentTools(t, ts.URL, agentToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"list_services", "get_logs", "get_metric", "check_deploy_health"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s missing from %v", want, names)
		}
	}
	if slices.Contains(names, "restart_service") {
		t.Errorf("restart_service exposed to the agent: %v", names)
	}
}

func TestAgentMCPWithoutLogs(t *testing.T) {
	ts := agentServer(t, false)
	names, err := agentTools(t, ts.URL, agentToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range logTools {
		if slices.Contains(names, tool) {
			t.Errorf("%s exposed with RIKUGAN_ESCALATION_LOGS=off", tool)
		}
	}
	if !slices.Contains(names, "get_metric") {
		t.Errorf("get_metric missing from %v", names)
	}
}

func TestAgentMCPReadsDockerLogsWithoutLoki(t *testing.T) {
	t.Setenv("ORCHESTRATION_PROVIDER", "dockercompose")
	t.Setenv("LOKI_URL", "")
	t.Setenv("PROMETHEUS_URL", "")
	srv, err := setupAgent(AgentMCPConfig{Token: agentToken, Logs: true}, nil, discard(), "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(AgentHandler(srv, agentToken))
	t.Cleanup(ts.Close)
	names, err := agentTools(t, ts.URL, agentToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"get_logs", "tail_logs", "search_logs"} {
		if !slices.Contains(names, want) {
			t.Errorf("%s missing from %v", want, names)
		}
	}
	if slices.Contains(names, "detect_log_anomalies") {
		t.Errorf("Loki's detect_log_anomalies exposed without Loki: %v", names)
	}
}

func TestAgentMCPRequiresItsToken(t *testing.T) {
	ts := agentServer(t, true)
	for _, token := range []string{"", "the-verdicts-api-token-not-the-agent-one"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+AgentMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
	if _, err := agentTools(t, ts.URL, "wrong"+agentToken[5:]); err == nil {
		t.Error("an MCP client with a wrong token got through")
	}
}

func TestBoundOutputs(t *testing.T) {
	long := strings.Repeat("line\n", 300)
	handler := func(text string) server.ToolHandlerFunc {
		return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(text), nil
		}
	}
	call := func(tool, text string, maxBytes, maxLines int) string {
		req := mcp.CallToolRequest{}
		req.Params.Name = tool
		res, err := boundOutputs(maxBytes, maxLines)(handler(text))(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(mcp.TextContent).Text
	}

	if got := call("get_logs", long, 1<<20, 200); strings.Count(got, "line\n") != 200 || !strings.Contains(got, "200 of 301 lines") {
		t.Errorf("log lines not capped: %d lines, tail %q", strings.Count(got, "line\n"), got[len(got)-60:])
	}
	if got := call("get_metric", long, 1<<20, 200); got != long {
		t.Error("a non-log tool had its lines capped")
	}
	// Cut on a rune boundary: "é" is two bytes.
	got := call("get_metric", strings.Repeat("é", 10), 5, 200)
	if !strings.HasPrefix(got, "éé\n[truncated by rikugan: 4 of 20 bytes]") {
		t.Errorf("byte cap = %q", got)
	}
}
