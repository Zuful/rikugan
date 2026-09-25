// Package provider defines the contract that every backend (Kubernetes, Prometheus, …)
// must satisfy in order to expose its capabilities as MCP tools.
package provider

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ToolDefinition couples an MCP tool schema with its handler so that
// registration is always a single, self-contained unit.
type ToolDefinition struct {
	Tool    mcp.Tool
	Handler server.ToolHandlerFunc
}

// Provider is the interface every backend must implement.
type Provider interface {
	// Name returns a unique, human-readable identifier (e.g. "kubernetes").
	Name() string

	// Tools returns the set of MCP tools this provider exposes.
	Tools() []ToolDefinition
}
