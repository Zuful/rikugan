package app

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/pkg/provider"
)

type writeProvider struct{}

func (writeProvider) Name() string { return "orchestration" }
func (writeProvider) Tools() []provider.ToolDefinition {
	h := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }
	return []provider.ToolDefinition{
		{Tool: mcp.NewTool("list_services"), Handler: h},
		{Tool: mcp.NewTool("restart_service"), Handler: h},
	}
}

func TestReadOnlyFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value   string
		restart bool
		fails   bool
	}{
		{"", true, false},
		{"false", true, false},
		{"true", false, false},
		{"yes", false, true},
		{"TRUE", false, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RIKUGAN_READ_ONLY", tc.value)
			srv := server.NewMCPServer("test", "0")
			d, err := newDispatcher(srv, discard())
			if tc.fails {
				if err == nil {
					t.Fatal("accepted a value that is neither true nor false")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Register(writeProvider{}); err != nil {
				t.Fatal(err)
			}
			if got := srv.GetTool("restart_service") != nil; got != tc.restart {
				t.Errorf("restart_service registered = %v, want %v", got, tc.restart)
			}
			if srv.GetTool("list_services") == nil {
				t.Error("list_services missing")
			}
		})
	}
}
