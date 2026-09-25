// Package dispatcher is the central registry that connects Provider
// implementations to the MCP server by registering their tools.
package dispatcher

import (
	"log/slog"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/pkg/provider"
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

// Register adds a provider and registers all of its tools on the MCP
// server.
//
// A provider with zero direct tools is accepted. This is the normal
// shape for backends that only project into a concept interface (e.g.
// orchestration/dockercompose, alerts/alertmanager): the concept's
// ConceptProvider exposes the canonical MCP tools and the backend is
// registered purely so it appears in Providers() and gets a startup
// log line. Previously rejecting them was a latent bug that blocked
// Docker Compose runtime wiring.
func (d *Dispatcher) Register(p provider.Provider) error {
	tools := p.Tools()

	for _, td := range tools {
		d.server.AddTool(td.Tool, td.Handler)
		d.logger.Info("registered tool",
			slog.String("provider", p.Name()),
			slog.String("tool", td.Tool.Name),
		)
	}

	d.providers = append(d.providers, p)
	if len(tools) == 0 {
		d.logger.Info("provider registered (no direct tools — exposed via concept interface)",
			slog.String("provider", p.Name()),
		)
	} else {
		d.logger.Info("provider registered",
			slog.String("provider", p.Name()),
			slog.Int("tools", len(tools)),
		)
	}

	return nil
}

// Providers returns the list of currently registered providers.
func (d *Dispatcher) Providers() []provider.Provider {
	return d.providers
}
