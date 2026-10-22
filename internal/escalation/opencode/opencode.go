// Package opencode drives an OpenCode server (opencode serve) through
// its HTTP API, for the investigation of a critical: one session per
// investigation, a prompt whose answer must follow a JSON Schema, and
// the session's tool calls, tokens and cost read back afterwards.
//
// The answer is asked for as JSON in the agent's last message, not
// through OpenCode's structured output ("format": json_schema): in
// 1.19.3, a session whose prompt carries a format can no longer be
// listed (GET /session/{id}/message answers 400, the stored format
// failing its own response schema), and the StructuredOutput tool it
// adds would read as a native tool call. rikugan validates the answer
// anyway.
//
// The server is a code execution surface (shell, terminal, file reads,
// config changes are all in its API), so the client requires its
// password, and Preflight refuses to go on unless every native tool can
// be switched off and the only MCP server is rikugan's.
//
// Written against OpenCode 1.19.3.
package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Zuful/rikugan/internal/escalation"
	"github.com/Zuful/rikugan/pkg/restclient"
)

// MCPServer is the name rikugan's MCP server has in the generated
// OpenCode config; OpenCode prefixes its tools with it.
const MCPServer = "rikugan"

// KnownNativeTools are the native tools of the OpenCode versions this
// client was checked against. An unknown one blocks the escalation: a
// new version must not open a tool silently.
var KnownNativeTools = []string{
	"invalid", "question", "bash", "read", "glob", "grep", "edit", "write", "task",
	"webfetch", "todowrite", "todoread", "websearch", "skill", "apply_patch",
	"list", "patch", "multiedit", "lsp", "codesearch",
}

// Client talks to one OpenCode server.
type Client struct {
	base string
	rest *restclient.Client
	// Poll is the interval between status checks while a session runs.
	Poll time.Duration
}

// New returns a client for the server at baseURL; password is its
// OPENCODE_SERVER_PASSWORD, required.
func New(baseURL, password string, opts ...restclient.Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("opencode: the server URL must be http(s)://host:port")
	}
	if password == "" {
		return nil, errors.New("opencode: the server password is required: an OpenCode server without one runs anyone's commands")
	}
	auth := base64.StdEncoding.EncodeToString([]byte("opencode:" + password))
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		rest: restclient.New("opencode", append([]restclient.Option{
			restclient.WithHeader("Authorization", "Basic "+auth),
			restclient.WithStatusHint(http.StatusUnauthorized, "opencode refused the password (HTTP 401): check OPENCODE_SERVER_PASSWORD"),
		}, opts...)...),
		Poll: 2 * time.Second,
	}, nil
}

// Preflight checks the server before an investigation runs:
//
//   - the native tool inventory can be read and holds no tool outside
//     KnownNativeTools: an unknown tool, or an answer of another shape,
//     is an error;
//   - the server's MCP servers are rikugan's alone: another one, added
//     through the API, could bring tools of its own;
//   - rikugan's is connected. OpenCode does not retry a connection that
//     failed (rikugan serve not up yet when it started, a restart since),
//     so Preflight asks for one; without it the agent would have no
//     tools at all.
//
// It returns the native tools, all of which Run switches off.
func (c *Client) Preflight(ctx context.Context) ([]string, error) {
	var ids []string
	if err := c.rest.GetJSON(ctx, c.base+"/experimental/tool/ids", &ids); err != nil {
		return nil, fmt.Errorf("native tools cannot be listed: %w", err)
	}
	if len(ids) == 0 {
		return nil, errors.New("native tools: the server listed none, which this client cannot verify")
	}
	var unknown []string
	for _, id := range ids {
		if !slices.Contains(KnownNativeTools, id) && !strings.HasPrefix(id, MCPServer+"_") {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("native tools unknown to this version of rikugan: %s", strings.Join(unknown, ", "))
	}
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if err := c.rest.GetJSON(ctx, c.base+"/config", &cfg); err != nil {
		return nil, fmt.Errorf("server config cannot be read: %w", err)
	}
	var others []string
	for name := range cfg.MCP {
		if name != MCPServer {
			others = append(others, name)
		}
	}
	if _, ok := cfg.MCP[MCPServer]; !ok || len(others) > 0 {
		sort.Strings(others)
		return nil, fmt.Errorf("the server's MCP servers must be %q alone (found others: %v)", MCPServer, others)
	}
	if err := c.connected(ctx); err != nil {
		return nil, err
	}
	var natives []string
	for _, id := range ids {
		if !strings.HasPrefix(id, MCPServer+"_") {
			natives = append(natives, id)
		}
	}
	return natives, nil
}

// connected checks that rikugan's MCP server is connected, asking for a
// connection once when it is not.
func (c *Client) connected(ctx context.Context) error {
	type status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	read := func() (status, error) {
		var all map[string]status
		if err := c.rest.GetJSON(ctx, c.base+"/mcp", &all); err != nil {
			return status{}, fmt.Errorf("MCP status cannot be read: %w", err)
		}
		return all[MCPServer], nil
	}
	st, err := read()
	if err != nil || st.Status == "connected" {
		return err
	}
	if err := c.rest.PostJSON(ctx, c.base+"/mcp/"+MCPServer+"/connect", struct{}{}, nil); err != nil {
		return fmt.Errorf("rikugan's MCP server is %s and reconnecting failed: %w", orUnknown(st.Status), err)
	}
	if st, err = read(); err != nil || st.Status == "connected" {
		return err
	}
	msg := fmt.Sprintf("rikugan's MCP server is not connected to OpenCode (%s", orUnknown(st.Status))
	if st.Error != "" {
		msg += ": " + st.Error
	}
	return errors.New(msg + "): check RIKUGAN_AGENT_MCP_TOKEN and that OpenCode reaches rikugan serve's agent MCP port")
}

func orUnknown(s string) string {
	if s == "" {
		return "status unknown"
	}
	return s
}

// Request is an investigation to run.
type Request struct {
	Title  string
	System string
	Prompt string
	// Schema is the JSON Schema the answer must follow, given to the
	// agent with the prompt.
	Schema map[string]any
	// Natives are switched off for the prompt (from Preflight).
	Natives []string
	// Timeout and MaxCalls bound the session: past either, it is
	// aborted. MaxTokens too, as the session's running total.
	Timeout   time.Duration
	MaxCalls  int
	MaxTokens int
}

// Result is what an investigation produced.
type Result struct {
	SessionID string
	// Answer is the JSON object of the agent's last message, nil when
	// there is none.
	Answer json.RawMessage
	// noAnswer says what the last message lacks when Answer is nil.
	noAnswer string
	// Calls are the tool calls, rikugan's tools under their own names.
	Calls []escalation.ToolCall
	// Native are the native tools the agent called anyway: any makes the
	// result untrustworthy.
	Native []string
	Tokens int
	// Cost in dollars, negative when the server gave none (or only
	// zero for tokens spent: a model without a known price).
	Cost float64
	// Stopped says why the session was aborted: "timeout", "calls",
	// "tokens"; empty when it ended by itself.
	Stopped string
	// Err is the error the agent's model returned, if any.
	Err string
}

// answerFormat follows the prompt: the shape of the answer.
const answerFormat = "\n\nWhen you are done, answer with one JSON object and nothing else: no prose, no code fence. It must follow this JSON Schema:\n"

// reminder is sent once when the agent ended without such an answer.
const reminder = "Your last message is not the answer required (%s). Answer again with the JSON object alone, following the JSON Schema given earlier. Do not call any tool."

// Run creates a session, sends the prompt and waits for the session to
// end, aborting it past the request's bounds, then reads back what it
// did. An agent that ends without a JSON answer is reminded of it once,
// within the same bounds. The session is left on the server, so that a
// person can open it.
func (c *Client) Run(ctx context.Context, req Request) (Result, error) {
	var sess struct {
		ID string `json:"id"`
	}
	if err := c.rest.PostJSON(ctx, c.base+"/session", map[string]any{
		"title": req.Title,
		// Deny everything but rikugan's MCP tools; the prompt also
		// switches every native tool off.
		"permission": []map[string]string{
			{"permission": "*", "pattern": "*", "action": "deny"},
			{"permission": MCPServer + "_*", "pattern": "*", "action": "allow"},
		},
	}, &sess); err != nil {
		return Result{}, fmt.Errorf("create session: %w", err)
	}
	if sess.ID == "" {
		return Result{}, errors.New("create session: no session id in the answer")
	}
	schema, err := json.Marshal(req.Schema)
	if err != nil {
		return Result{SessionID: sess.ID}, fmt.Errorf("answer schema: %w", err)
	}
	deadline := time.Now().Add(req.Timeout)
	if err := c.prompt(ctx, sess.ID, req, req.Prompt+answerFormat+string(schema)); err != nil {
		return Result{SessionID: sess.ID}, err
	}
	stopped := c.wait(ctx, sess.ID, req, deadline)
	res, err := c.collect(context.WithoutCancel(ctx), sess.ID)
	if err == nil && stopped == "" && res.Answer == nil && res.Err == "" && len(res.Native) == 0 {
		if err := c.prompt(ctx, sess.ID, req, fmt.Sprintf(reminder, res.noAnswer)); err != nil {
			return res, err
		}
		stopped = c.wait(ctx, sess.ID, req, deadline)
		res, err = c.collect(context.WithoutCancel(ctx), sess.ID)
	}
	res.Stopped = stopped
	return res, err
}

// prompt sends text to the session, every native tool off.
func (c *Client) prompt(ctx context.Context, id string, req Request, text string) error {
	tools := map[string]bool{}
	for _, n := range req.Natives {
		tools[n] = false
	}
	if err := c.rest.PostJSON(ctx, c.base+"/session/"+url.PathEscape(id)+"/prompt_async", map[string]any{
		"system": req.System,
		"tools":  tools,
		"parts":  []map[string]string{{"type": "text", "text": text}},
	}, nil); err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}
	return nil
}

// wait polls the session until it is idle, aborting it when a bound is
// passed; it returns the bound, or "" when the session ended by itself.
func (c *Client) wait(ctx context.Context, id string, req Request, deadline time.Time) string {
	abort := func(why string) string {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = c.rest.PostJSON(actx, c.base+"/session/"+url.PathEscape(id)+"/abort", struct{}{}, nil)
		return why
	}
	for {
		select {
		case <-ctx.Done():
			return abort("canceled")
		case <-time.After(c.Poll):
		}
		if time.Now().After(deadline) {
			return abort("timeout")
		}
		var statuses map[string]struct {
			Type string `json:"type"`
		}
		if err := c.rest.GetJSON(ctx, c.base+"/session/status", &statuses); err == nil {
			if st, ok := statuses[id]; !ok || st.Type == "idle" {
				return ""
			}
		}
		if req.MaxCalls > 0 || req.MaxTokens > 0 {
			if res, err := c.collect(ctx, id); err == nil {
				if req.MaxCalls > 0 && len(res.Calls)+len(res.Native) > req.MaxCalls {
					return abort("calls")
				}
				if req.MaxTokens > 0 && res.Tokens > req.MaxTokens {
					return abort("tokens")
				}
			}
		}
	}
}

type part struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Tool   string `json:"tool"`
	Tokens *struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		Reasoning float64 `json:"reasoning"`
	} `json:"tokens"`
	Cost  *float64 `json:"cost"`
	State struct {
		Status string `json:"status"`
		Output string `json:"output"`
		Error  string `json:"error"`
	} `json:"state"`
}

type message struct {
	Info struct {
		Role  string `json:"role"`
		Error *struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
	} `json:"info"`
	Parts []part `json:"parts"`
}

// collect reads the session's messages: tool calls, the answer in the
// last message with text, and tokens and cost summed over the steps.
func (c *Client) collect(ctx context.Context, id string) (Result, error) {
	res := Result{SessionID: id, Cost: -1}
	var msgs []message
	if err := c.rest.GetJSON(ctx, c.base+"/session/"+url.PathEscape(id)+"/message", &msgs); err != nil {
		return res, fmt.Errorf("read session messages: %w", err)
	}
	last := ""
	for _, m := range msgs {
		if m.Info.Role != "assistant" {
			continue
		}
		var text strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				text.WriteString(p.Text)
			}
		}
		if strings.TrimSpace(text.String()) != "" {
			last = text.String()
		}
		if m.Info.Error != nil {
			res.Err = strings.TrimSpace(m.Info.Error.Name + ": " + m.Info.Error.Data.Message)
		}
		for _, p := range m.Parts {
			switch p.Type {
			case "tool":
				name, ok := strings.CutPrefix(p.Tool, MCPServer+"_")
				if !ok {
					res.Native = append(res.Native, p.Tool)
					continue
				}
				out := p.State.Output
				if p.State.Status == "error" {
					out = p.State.Error
				}
				res.Calls = append(res.Calls, escalation.ToolCall{Tool: name, Output: out})
			case "step-finish":
				if p.Tokens != nil {
					res.Tokens += int(p.Tokens.Input + p.Tokens.Output + p.Tokens.Reasoning)
				}
				if p.Cost != nil {
					if res.Cost < 0 {
						res.Cost = 0
					}
					res.Cost += *p.Cost
				}
			}
		}
	}
	if res.Cost == 0 && res.Tokens > 0 {
		res.Cost = -1 // tokens without a price: the model's cost is unknown
	}
	res.Answer, res.noAnswer = answer(last)
	return res, nil
}

// answer finds the JSON object in the agent's last message, which a
// model may wrap in a code fence or a sentence; otherwise it says what
// is wrong.
func answer(text string) (json.RawMessage, string) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	switch {
	case strings.TrimSpace(text) == "":
		return nil, "no text"
	case start < 0 || end < start:
		return nil, "no JSON object"
	}
	raw := json.RawMessage(text[start : end+1])
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "invalid JSON: " + err.Error()
	}
	return raw, ""
}
