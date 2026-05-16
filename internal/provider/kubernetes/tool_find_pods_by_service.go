package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// findPodsByServiceTool returns the ToolDefinition for find_pods_by_service_name.
func (p *Provider) findPodsByServiceTool() provider.ToolDefinition {
	tool := mcp.NewTool("find_pods_by_service_name",
		mcp.WithDescription(
			"Find Kubernetes pods that belong to a logical service name. "+
				"Bridges the gap between Jaeger service names and Kubernetes pod names. "+
				"Searches by common labels (app, app.kubernetes.io/name) first, "+
				"then falls back to pod name prefix matching. "+
				"[Methodology: cross-signal correlation and blast-radius scoping — see docs/methodology.md]",
		),
		mcp.WithString("service_name",
			mcp.Required(),
			mcp.Description("Logical service name (e.g. from a Jaeger trace)"),
		),
		mcp.WithString("namespace",
			mcp.Description("Kubernetes namespace to search in (default: all namespaces)"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleFindPodsByService,
	}
}

// handleFindPodsByService is the MCP handler for find_pods_by_service_name.
func (p *Provider) handleFindPodsByService(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	serviceName, err := req.RequireString("service_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	namespace, _ := req.RequireString("namespace")

	log := p.logger.With(slog.String("tool", "find_pods_by_service_name"))
	log.Info("tool called",
		"service_name", serviceName,
		"namespace", namespace,
	)

	type podMatch struct {
		Name      string
		Namespace string
		Phase     string
		Method    string // how it was matched
	}

	seen := map[string]bool{}
	var matches []podMatch

	// Phase 1: Label-based discovery with common labels.
	labelSelectors := []struct {
		selector string
		label    string
	}{
		{fmt.Sprintf("app=%s", serviceName), "app"},
		{fmt.Sprintf("app.kubernetes.io/name=%s", serviceName), "app.kubernetes.io/name"},
	}

	for _, ls := range labelSelectors {
		pods, listErr := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: ls.selector,
		})
		if listErr != nil {
			log.Warn("label query failed", "selector", ls.selector, "error", listErr.Error())
			continue
		}
		for _, pod := range pods.Items {
			key := pod.Namespace + "/" + pod.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			matches = append(matches, podMatch{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				Phase:     string(pod.Status.Phase),
				Method:    fmt.Sprintf("label:%s", ls.label),
			})
		}
	}

	// Phase 2: Name-prefix fallback (only if labels found nothing).
	if len(matches) == 0 {
		pods, listErr := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("kubernetes API error: %v", listErr)), nil
		}
		for _, pod := range pods.Items {
			if strings.HasPrefix(pod.Name, serviceName) {
				key := pod.Namespace + "/" + pod.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				matches = append(matches, podMatch{
					Name:      pod.Name,
					Namespace: pod.Namespace,
					Phase:     string(pod.Status.Phase),
					Method:    "name-prefix",
				})
			}
		}
	}

	// Build report.
	var b strings.Builder

	fmt.Fprintf(&b, "## Pods for Service `%s`\n\n", serviceName)
	if namespace != "" {
		fmt.Fprintf(&b, "**Namespace:** `%s`\n\n", namespace)
	} else {
		b.WriteString("**Scope:** all namespaces\n\n")
	}

	if len(matches) == 0 {
		b.WriteString("_No pods found matching this service name._\n\n")
		b.WriteString("**Suggestions:**\n")
		b.WriteString("- Check if the service name in Jaeger matches the Kubernetes labels\n")
		b.WriteString("- Try specifying a namespace to narrow the search\n")
		b.WriteString("- The service may use a different naming convention\n")
		return mcp.NewToolResultText(b.String()), nil
	}

	fmt.Fprintf(&b, "**Pods found:** %d\n\n", len(matches))
	b.WriteString("| Pod | Namespace | Phase | Match Method |\n")
	b.WriteString("|-----|-----------|-------|--------------|\n")

	for _, m := range matches {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n",
			m.Name, m.Namespace, m.Phase, m.Method,
		)
	}

	b.WriteString("\n**Next steps:** Use `diagnose_pod` or `six_eyes` with any of the pod names above.\n")

	return mcp.NewToolResultText(b.String()), nil
}
