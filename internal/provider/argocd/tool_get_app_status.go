package argocd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"
)

const opTimeout = 30 * time.Second

// getAppStatusTool returns the ToolDefinition for get_app_status.
func (p *Provider) getAppStatusTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_app_status",
		mcp.WithDescription(
			"Get the sync and health status of an Argo CD application. "+
				"Returns sync state, health state, tracked repository, target revision, "+
				"and any error messages. Useful for determining if a GitOps deployment "+
				"succeeded or is out of sync. "+
				"[Methodology: Step 0 (what changed) — see docs/methodology.md]",
		),
		mcp.WithString("app_name",
			mcp.Required(),
			mcp.Description("Name of the Argo CD application"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetAppStatus,
	}
}

// handleGetAppStatus is the MCP handler for get_app_status.
func (p *Provider) handleGetAppStatus(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	appName, err := req.RequireString("app_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	log := p.logger.With(slog.String("tool", "get_app_status"))
	log.Info("tool called", "app_name", appName)

	app, err := p.client.GetApp(ctx, appName)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("argocd API error: %v", err)), nil
	}

	report := buildAppStatusReport(app)
	return mcp.NewToolResultText(report), nil
}

func buildAppStatusReport(app *Application) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Argo CD Application Status\n\n")
	fmt.Fprintf(&b, "**Application:** `%s`\n\n", app.Metadata.Name)

	fmt.Fprintf(&b, "| Field | Value |\n")
	fmt.Fprintf(&b, "|-------|-------|\n")
	fmt.Fprintf(&b, "| Sync Status | **%s** |\n", app.Status.Sync.Status)
	fmt.Fprintf(&b, "| Health Status | **%s** |\n", app.Status.Health.Status)
	fmt.Fprintf(&b, "| Project | %s |\n", app.Spec.Project)
	fmt.Fprintf(&b, "| Repository | `%s` |\n", app.Spec.Source.RepoURL)
	fmt.Fprintf(&b, "| Target Revision | `%s` |\n", app.Spec.Source.TargetRevision)
	fmt.Fprintf(&b, "| Path | `%s` |\n", app.Spec.Source.Path)
	fmt.Fprintf(&b, "| Synced Revision | `%s` |\n", app.Status.Sync.Revision)

	if app.Status.Health.Message != "" {
		fmt.Fprintf(&b, "\n### Health Message\n\n```\n%s\n```\n\n", app.Status.Health.Message)
	}

	if op := app.Status.OperationState; op != nil {
		fmt.Fprintf(&b, "\n### Last Operation\n\n")
		fmt.Fprintf(&b, "| Field | Value |\n")
		fmt.Fprintf(&b, "|-------|-------|\n")
		fmt.Fprintf(&b, "| Phase | %s |\n", op.Phase)
		if op.SyncResult != nil {
			fmt.Fprintf(&b, "| Sync Revision | `%s` |\n", op.SyncResult.Revision)
		}
		if op.Message != "" {
			fmt.Fprintf(&b, "\n**Message:**\n```\n%s\n```\n\n", op.Message)
		}
	}

	return b.String()
}
