package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an OpenCode server as far as the client goes: the
// shapes of 1.19.3's answers.
type fakeServer struct {
	mu        sync.Mutex
	toolIDs   any
	mcp       map[string]any
	busyPolls int // status polls answered busy before idle
	messages  any
	// seen
	session map[string]any
	prompt  map[string]any
	aborted bool
	polls   int
}

func (f *fakeServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if user, pass, ok := r.BasicAuth(); !ok || user != "opencode" || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		decode := func() map[string]any {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			return m
		}
		switch {
		case r.URL.Path == "/experimental/tool/ids":
			if f.toolIDs == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(f.toolIDs)
		case r.URL.Path == "/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"mcp": f.mcp})
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			f.session = decode()
			_, _ = w.Write([]byte(`{"id":"ses_1"}`))
		case r.URL.Path == "/session/ses_1/prompt_async":
			f.prompt = decode()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/session/status":
			f.polls++
			if f.polls <= f.busyPolls {
				_, _ = w.Write([]byte(`{"ses_1":{"type":"busy"}}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/session/ses_1/message":
			_ = json.NewEncoder(w).Encode(f.messages)
		case r.URL.Path == "/session/ses_1/abort":
			f.aborted = true
			_, _ = w.Write([]byte(`true`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func client(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "pw")
	if err != nil {
		t.Fatal(err)
	}
	c.Poll = time.Millisecond
	return c
}

var natives = []any{"invalid", "question", "bash", "read", "glob", "grep", "edit", "write", "task", "webfetch", "todowrite", "websearch", "skill", "apply_patch"}

func TestNewRequiresAPassword(t *testing.T) {
	if _, err := New("http://opencode:4096", ""); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("err = %v", err)
	}
	if _, err := New("opencode:4096", "pw"); err == nil {
		t.Error("accepted a URL without a scheme")
	}
}

func TestPreflight(t *testing.T) {
	ok := map[string]any{"rikugan": map[string]any{"type": "remote"}}
	for _, tc := range []struct {
		name  string
		ids   any
		mcp   map[string]any
		fails string
	}{
		{name: "the 1.19.3 inventory", ids: natives, mcp: ok},
		{name: "a tool this version does not know", ids: append(append([]any{}, natives...), "browser"), mcp: ok, fails: "unknown to this version of rikugan: browser"},
		{name: "the endpoint is gone", ids: nil, mcp: ok, fails: "cannot be listed"},
		{name: "the answer changed shape", ids: map[string]any{"tools": natives}, mcp: ok, fails: "cannot be listed"},
		{name: "an empty inventory", ids: []any{}, mcp: ok, fails: "listed none"},
		{name: "another MCP server", ids: natives, mcp: map[string]any{"rikugan": 1, "filesystem": 1}, fails: "filesystem"},
		{name: "no rikugan MCP server", ids: natives, mcp: map[string]any{}, fails: `"rikugan" alone`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client(t, &fakeServer{toolIDs: tc.ids, mcp: tc.mcp}).Preflight(context.Background())
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatalf("err = %v, want %q", err, tc.fails)
				}
				return
			}
			if err != nil || len(got) != len(natives) {
				t.Fatalf("natives %v, err %v", got, err)
			}
		})
	}
}

func TestWrongPassword(t *testing.T) {
	srv := httptest.NewServer((&fakeServer{}).handler(t))
	defer srv.Close()
	c, _ := New(srv.URL, "not-it")
	if _, err := c.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENCODE_SERVER_PASSWORD") {
		t.Errorf("err = %v", err)
	}
}

const messages = `[
 {"info":{"role":"user"},"parts":[{"type":"text","text":"investigate"}]},
 {"info":{"role":"assistant","structured":null},"parts":[
   {"type":"tool","tool":"rikugan_get_logs","state":{"status":"completed","output":"panic: nil map write"}},
   {"type":"tool","tool":"rikugan_get_metric","state":{"status":"error","error":"metrics backend down"}},
   {"type":"step-finish","tokens":{"input":1000,"output":200,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.01}]},
 {"info":{"role":"assistant","structured":{"cause":"nil map","confidence":"high"}},"parts":[
   {"type":"tool","tool":"bash","state":{"status":"completed","output":"root"}},
   {"type":"step-finish","tokens":{"input":2000,"output":300,"reasoning":100,"cache":{"read":0,"write":0}},"cost":0.02}]}
]`

func TestRun(t *testing.T) {
	f := &fakeServer{busyPolls: 2}
	_ = json.Unmarshal([]byte(messages), &f.messages)
	c := client(t, f)
	res, err := c.Run(context.Background(), Request{
		Title: "prod/api critical", System: "sys", Prompt: "investigate",
		Schema: map[string]any{"type": "object"}, Natives: []string{"bash", "read"},
		Timeout: time.Minute, MaxCalls: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "ses_1" || res.Stopped != "" || f.aborted {
		t.Errorf("session %q stopped %q aborted %v", res.SessionID, res.Stopped, f.aborted)
	}
	if string(res.Answer) != `{"cause":"nil map","confidence":"high"}` {
		t.Errorf("answer %s", res.Answer)
	}
	if len(res.Calls) != 2 || res.Calls[0].Tool != "get_logs" || res.Calls[0].Output != "panic: nil map write" || res.Calls[1].Output != "metrics backend down" {
		t.Errorf("calls %+v", res.Calls)
	}
	if len(res.Native) != 1 || res.Native[0] != "bash" {
		t.Errorf("native calls %v, want [bash]", res.Native)
	}
	if res.Tokens != 3600 || res.Cost < 0.0299 || res.Cost > 0.0301 {
		t.Errorf("tokens %d cost %v", res.Tokens, res.Cost)
	}

	// What the server was asked: deny all but rikugan's tools, natives
	// off, the answer's schema.
	perm, _ := json.Marshal(f.session["permission"])
	if string(perm) != `[{"action":"deny","pattern":"*","permission":"*"},{"action":"allow","pattern":"*","permission":"rikugan_*"}]` {
		t.Errorf("session permission %s", perm)
	}
	tools, _ := json.Marshal(f.prompt["tools"])
	format, _ := json.Marshal(f.prompt["format"])
	if string(tools) != `{"bash":false,"read":false}` || !strings.Contains(string(format), `"type":"json_schema"`) {
		t.Errorf("prompt tools %s format %s", tools, format)
	}
}

func TestRunAbortsPastItsBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"timeout", Request{Timeout: 20 * time.Millisecond}, "timeout"},
		{"tool calls", Request{Timeout: time.Minute, MaxCalls: 2}, "calls"},
		{"tokens", Request{Timeout: time.Minute, MaxTokens: 1000}, "tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeServer{busyPolls: 1 << 30}
			_ = json.Unmarshal([]byte(messages), &f.messages)
			res, err := client(t, f).Run(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Stopped != tc.want || !f.aborted {
				t.Errorf("stopped %q aborted %v, want %q", res.Stopped, f.aborted, tc.want)
			}
		})
	}
}

// A model without a known price spends tokens at zero cost: the cost is
// unknown, not free.
func TestUnknownCost(t *testing.T) {
	f := &fakeServer{}
	_ = json.Unmarshal([]byte(strings.ReplaceAll(strings.ReplaceAll(messages, `"cost":0.01`, `"cost":0`), `"cost":0.02`, `"cost":0`)), &f.messages)
	res, err := client(t, f).Run(context.Background(), Request{Timeout: time.Minute})
	if err != nil || res.Cost >= 0 {
		t.Errorf("cost %v err %v, want negative", res.Cost, err)
	}
}

// The chart switches off the same native tools this client knows: a tool
// added here must be off there too.
func TestChartSwitchesOffEveryKnownNativeTool(t *testing.T) {
	raw, err := os.ReadFile("../../../helm/rikugan/templates/escalation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\$natives := list ([^}]*)}}`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("no $natives list in the chart's escalation template")
	}
	chart := strings.Fields(strings.ReplaceAll(string(m[1]), `"`, ""))
	if strings.Join(chart, ",") != strings.Join(KnownNativeTools, ",") {
		t.Errorf("chart natives %v\nclient knows %v", chart, KnownNativeTools)
	}
}
