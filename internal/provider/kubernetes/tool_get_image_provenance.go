package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Zuful/rikugan/internal/provider"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var gitSHAPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// getImageProvenanceTool returns the ToolDefinition for get_image_provenance.
func (p *Provider) getImageProvenanceTool() provider.ToolDefinition {
	tool := mcp.NewTool("get_image_provenance",
		mcp.WithDescription(
			"Extract container image tags from a Kubernetes pod and identify "+
				"potential Git commit SHAs in the tags. Bridges the gap between "+
				"a running pod and its source code — if a tag looks like a Git SHA, "+
				"the report tells the LLM exactly which tool to call next.",
		),
		mcp.WithString("namespace",
			mcp.Required(),
			mcp.Description("Kubernetes namespace of the pod"),
		),
		mcp.WithString("pod_name",
			mcp.Required(),
			mcp.Description("Name of the pod to inspect"),
		),
	)

	return provider.ToolDefinition{
		Tool:    tool,
		Handler: p.handleGetImageProvenance,
	}
}

// handleGetImageProvenance is the MCP handler for get_image_provenance.
func (p *Provider) handleGetImageProvenance(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	namespace, err := req.RequireString("namespace")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	podName, err := req.RequireString("pod_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	log := p.logger.With(slog.String("tool", "get_image_provenance"))
	log.Info("tool called",
		"namespace", namespace,
		"pod_name", podName,
	)

	pod, err := p.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("kubernetes API error: %v", err)), nil
	}

	type containerImage struct {
		Container string
		Image     string
		Registry  string
		Repo      string
		Tag       string
		IsGitSHA  bool
	}

	var images []containerImage
	for _, c := range pod.Spec.Containers {
		ci := parseImage(c.Name, c.Image)
		images = append(images, ci)
	}
	for _, c := range pod.Spec.InitContainers {
		ci := parseImage(c.Name+" (init)", c.Image)
		images = append(images, ci)
	}

	// Build report.
	var b strings.Builder

	fmt.Fprintf(&b, "## Image Provenance\n\n")
	fmt.Fprintf(&b, "**Pod:** `%s`  \n", podName)
	fmt.Fprintf(&b, "**Namespace:** `%s`\n\n", namespace)

	b.WriteString("| Container | Image | Tag | Git SHA? |\n")
	b.WriteString("|-----------|-------|-----|----------|\n")

	for _, ci := range images {
		shaIndicator := "No"
		if ci.IsGitSHA {
			shaIndicator = "**Yes**"
		}
		repoDisplay := ci.Image
		if ci.Registry != "" {
			repoDisplay = ci.Registry + "/" + ci.Repo
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n",
			ci.Container, repoDisplay, ci.Tag, shaIndicator,
		)
	}
	b.WriteString("\n")

	// Actionable suggestions for the LLM.
	var shaImages []containerImage
	for _, ci := range images {
		if ci.IsGitSHA {
			shaImages = append(shaImages, ci)
		}
	}

	if len(shaImages) > 0 {
		fmt.Fprintf(&b, "### Detected Git SHAs\n\n")
		for _, ci := range shaImages {
			fmt.Fprintf(&b, "- Container `%s` has tag `%s` which looks like a Git commit SHA.\n",
				ci.Container, ci.Tag,
			)
			fmt.Fprintf(&b, "  → To see the code: use `get_commit_diff` with `commit_sha: \"%s\"`\n\n",
				ci.Tag,
			)
		}
	} else {
		b.WriteString("_No Git SHA tags detected._ Tags may be semantic versions or other formats.\n")
		b.WriteString("Check `get_app_status` in Argo CD for the deployed revision, or `get_recent_commits` for recent changes.\n")
	}

	return mcp.NewToolResultText(b.String()), nil
}

// parseImage splits a container image reference into registry, repo, and tag.
func parseImage(containerName, image string) struct {
	Container string
	Image     string
	Registry  string
	Repo      string
	Tag       string
	IsGitSHA  bool
} {
	result := struct {
		Container string
		Image     string
		Registry  string
		Repo      string
		Tag       string
		IsGitSHA  bool
	}{
		Container: containerName,
		Image:     image,
	}

	// Split off tag (or digest).
	ref := image
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		// Digest reference — no meaningful tag to parse.
		result.Tag = ref[idx:]
		ref = ref[:idx]
	} else if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		result.Tag = ref[idx+1:]
		ref = ref[:idx]
	} else {
		result.Tag = "latest"
	}

	// Split registry from repo path.
	// If the first segment contains a dot or colon, it's a registry.
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		result.Registry = parts[0]
		result.Repo = parts[1]
	} else {
		result.Repo = ref
	}

	// Check if the tag looks like a Git SHA.
	result.IsGitSHA = gitSHAPattern.MatchString(result.Tag)

	return result
}
