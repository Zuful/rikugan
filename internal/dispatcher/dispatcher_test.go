package dispatcher

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/provider"
)

// stubProvider is a minimal Provider for testing.
type stubProvider struct {
	name  string
	tools []provider.ToolDefinition
}

func (s *stubProvider) Name() string                   { return s.name }
func (s *stubProvider) Tools() []provider.ToolDefinition { return s.tools }

func noopHandler(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("ok"), nil
}

func newTestDispatcher() *Dispatcher {
	srv := server.NewMCPServer("test", "0.0.0")
	return New(srv, slog.Default())
}

func TestRegister_ValidProvider(t *testing.T) {
	d := newTestDispatcher()
	p := &stubProvider{
		name: "test-provider",
		tools: []provider.ToolDefinition{
			{
				Tool:    mcp.NewTool("test_tool", mcp.WithDescription("a test tool")),
				Handler: noopHandler,
			},
		},
	}

	if err := d.Register(p); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(d.Providers()) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(d.Providers()))
	}
	if d.Providers()[0].Name() != "test-provider" {
		t.Errorf("expected provider name %q, got %q", "test-provider", d.Providers()[0].Name())
	}
}

func TestRegister_EmptyProvider(t *testing.T) {
	d := newTestDispatcher()
	p := &stubProvider{
		name:  "empty-provider",
		tools: nil,
	}

	err := d.Register(p)
	if err == nil {
		t.Fatal("expected error for provider with no tools")
	}
}

func TestRegister_MultipleProviders(t *testing.T) {
	d := newTestDispatcher()

	for _, name := range []string{"alpha", "beta"} {
		p := &stubProvider{
			name: name,
			tools: []provider.ToolDefinition{
				{
					Tool:    mcp.NewTool(name+"_tool", mcp.WithDescription("tool for "+name)),
					Handler: noopHandler,
				},
			},
		}
		if err := d.Register(p); err != nil {
			t.Fatalf("unexpected error registering %s: %v", name, err)
		}
	}

	if len(d.Providers()) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(d.Providers()))
	}
}

func TestProviders_EmptyByDefault(t *testing.T) {
	d := newTestDispatcher()
	if len(d.Providers()) != 0 {
		t.Fatalf("expected 0 providers, got %d", len(d.Providers()))
	}
}
