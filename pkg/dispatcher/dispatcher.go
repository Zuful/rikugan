// Package dispatcher is the central registry that connects Provider
// implementations to the MCP server by registering their tools.
package dispatcher

import (
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Dispatcher accepts providers and wires their tools into the MCP server.
type Dispatcher struct {
	server    *server.MCPServer
	providers []provider.Provider
	logger    *slog.Logger
	readOnly  bool
	withheld  map[string]bool
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithReadOnly withholds every tool not classified Read: the server
// then cannot change anything, whatever its backends' credentials
// allow.
func WithReadOnly(on bool) Option {
	return func(d *Dispatcher) { d.readOnly = on }
}

// WithoutTools withholds the named tools, whatever their class.
func WithoutTools(names ...string) Option {
	return func(d *Dispatcher) {
		if d.withheld == nil {
			d.withheld = map[string]bool{}
		}
		for _, n := range names {
			d.withheld[n] = true
		}
	}
}

// New creates a Dispatcher bound to the given MCP server.
func New(srv *server.MCPServer, logger *slog.Logger, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		server: srv,
		logger: logger,
	}
	for _, o := range opts {
		o(d)
	}
	return d
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
//
// Each tool's MCP annotations follow its classification (see
// toolAccess). In read-only mode, a tool not classified Read is not
// registered.
func (d *Dispatcher) Register(p provider.Provider) error {
	tools := p.Tools()

	for _, td := range tools {
		access, known := AccessOf(td.Tool.Name)
		if !known {
			d.logger.Warn("tool not classified as read or write: treated as write",
				slog.String("provider", p.Name()),
				slog.String("tool", td.Tool.Name),
			)
		}
		if d.withheld[td.Tool.Name] {
			d.logger.Info("tool withheld by configuration",
				slog.String("provider", p.Name()),
				slog.String("tool", td.Tool.Name),
			)
			continue
		}
		if d.readOnly && access != Read {
			d.logger.Info("tool withheld: read-only mode",
				slog.String("provider", p.Name()),
				slog.String("tool", td.Tool.Name),
			)
			continue
		}
		td.Tool.Annotations.ReadOnlyHint = mcp.ToBoolPtr(access == Read)
		td.Tool.Annotations.DestructiveHint = mcp.ToBoolPtr(access != Read)
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
