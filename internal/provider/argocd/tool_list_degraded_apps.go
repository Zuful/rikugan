package argocd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

// listDegradedAppsTool returns the ToolDefinition for list_degraded_apps.
func (p *Provider) listDegradedAppsTool() provider.ToolDefinition {
	tool := mcp.NewTool("list_degraded_apps",
		mcp.WithDescription(
			"List all Argo CD applications that are currently Degraded or OutOfSync. "+
				"Useful for a quick health check across all GitOps deployments. "+
				"Optionally filter by Argo CD project.",
		),
		mcp.WithString("project",
			mcp.Description("Argo CD project name to filter by (default: all projects)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleListDegradedApps,
	}
}

// handleListDegradedApps is the MCP handler for list_degraded_apps.
func (p *Provider) handleListDegradedApps(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	project, _ := req.RequireString("project")

	log := p.logger.With(slog.String("tool", "list_degraded_apps"))
	log.Info("tool called", "project", project)

	apps, err := p.client.ListApps(ctx, project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("argocd API error: %v", err)), nil
	}

	var degraded []Application
	for _, app := range apps {
		if isDegraded(app) {
			degraded = append(degraded, app)
		}
	}

	report := buildDegradedAppsReport(project, degraded, len(apps))
	return mcp.NewToolResultText(report), nil
}

func isDegraded(app Application) bool {
	return app.Status.Health.Status == "Degraded" ||
		app.Status.Health.Status == "Missing" ||
		app.Status.Sync.Status == "OutOfSync"
}

func buildDegradedAppsReport(project string, degraded []Application, total int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Degraded / OutOfSync Applications\n\n")
	if project != "" {
		fmt.Fprintf(&b, "**Project:** `%s`\n\n", project)
	}
	fmt.Fprintf(&b, "**Total applications scanned:** %d  \n", total)
	fmt.Fprintf(&b, "**Unhealthy:** %d\n\n", len(degraded))

	if len(degraded) == 0 {
		b.WriteString("_All applications are healthy and in sync._\n")
		return b.String()
	}

	b.WriteString("| Application | Sync | Health | Repository | Message |\n")
	b.WriteString("|-------------|------|--------|------------|---------|\n")

	for _, app := range degraded {
		msg := app.Status.Health.Message
		if msg == "" && app.Status.OperationState != nil {
			msg = app.Status.OperationState.Message
		}
		// Truncate long messages for table readability.
		if len(msg) > 80 {
			msg = msg[:77] + "..."
		}

		fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` | %s |\n",
			app.Metadata.Name,
			app.Status.Sync.Status,
			app.Status.Health.Status,
			app.Spec.Source.RepoURL,
			msg,
		)
	}

	return b.String()
}
