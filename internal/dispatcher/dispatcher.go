// Package dispatcher is the central registry that connects Provider
// implementations to the MCP server by registering their tools.
package dispatcher

import (
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/provider"
)

// Dispatcher accepts providers and wires their tools into the MCP server.
type Dispatcher struct {
	server    *server.MCPServer
	providers []provider.Provider
	logger    *slog.Logger
}

// New creates a Dispatcher bound to the given MCP server.
func New(srv *server.MCPServer, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		server: srv,
		logger: logger,
	}
}

// Register adds a provider and registers all of its tools on the MCP server.
func (d *Dispatcher) Register(p provider.Provider) error {
	tools := p.Tools()
	if len(tools) == 0 {
		return fmt.Errorf("provider %q exposes no tools", p.Name())
	}

	for _, td := range tools {
		d.server.AddTool(td.Tool, td.Handler)
		d.logger.Info("registered tool",
			slog.String("provider", p.Name()),
			slog.String("tool", td.Tool.Name),
		)
	}

	d.providers = append(d.providers, p)
	d.logger.Info("provider registered",
		slog.String("provider", p.Name()),
		slog.Int("tools", len(tools)),
	)

	return nil
}

// Providers returns the list of currently registered providers.
func (d *Dispatcher) Providers() []provider.Provider {
	return d.providers
}

