// Package mcp wraps the mark3labs/mcp-go server construction in the
// defaults rikugan uses everywhere: no dynamic tool list changes,
// panic recovery inside handlers, a name/version pair injected from
// the caller. Future per-concept binaries call NewServer and stay
// uniform.
package mcp

import "github.com/mark3labs/mcp-go/server"

// NewServer builds an MCP server with rikugan's canonical defaults;
// opts add to them.
func NewServer(name, version string, opts ...server.ServerOption) *server.MCPServer {
	return server.NewMCPServer(
		name,
		version,
		append([]server.ServerOption{
			server.WithToolCapabilities(false), // no dynamic tool list changes yet
			server.WithRecovery(),              // recover panics inside handlers
		}, opts...)...,
	)
}
