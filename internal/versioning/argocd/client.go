package argocd

import (
	"context"
	"fmt"
	"strings"

	"github.com/Zuful/rikugan/pkg/restclient"
)

// Client is a thin wrapper around the Argo CD REST API.
type Client struct {
	baseURL string
	rest    *restclient.Client
}

// NewClient creates an Argo CD REST client.
func NewClient(serverURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(serverURL, "/"),
		rest: restclient.New("argocd",
			restclient.WithHeader("Authorization", "Bearer "+token),
		),
	}
}

// Application mirrors the subset of the Argo CD Application resource
// that our tools need. Unknown fields are silently ignored.
type Application struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Project string `json:"project"`
		Source  struct {
			RepoURL        string `json:"repoURL"`
			TargetRevision string `json:"targetRevision"`
			Path           string `json:"path"`
		} `json:"source"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"health"`
		OperationState *struct {
			Phase      string `json:"phase"`
			Message    string `json:"message"`
			SyncResult *struct {
				Revision string `json:"revision"`
			} `json:"syncResult"`
		} `json:"operationState"`
	} `json:"status"`
}

type applicationList struct {
	Items []Application `json:"items"`
}

// GetApp fetches a single application by name.
func (c *Client) GetApp(ctx context.Context, name string) (*Application, error) {
	url := fmt.Sprintf("%s/api/v1/applications/%s", c.baseURL, name)
	var app Application
	if err := c.rest.GetJSON(ctx, url, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// ListApps fetches all applications, optionally filtered by project.
func (c *Client) ListApps(ctx context.Context, project string) ([]Application, error) {
	url := fmt.Sprintf("%s/api/v1/applications", c.baseURL)
	if project != "" {
		url += "?project=" + project
	}
	var list applicationList
	if err := c.rest.GetJSON(ctx, url, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}
