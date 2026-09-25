package sentry

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/restclient"
)

// Client is a thin wrapper around the Sentry REST API (api/0/). Each Client
// is scoped to one organisation — Sentry's project and release endpoints
// are org-relative, so binding it once at construction keeps call sites
// compact.
type Client struct {
	baseURL string
	org     string
	rest    *restclient.Client
}

// NewClient creates a Sentry REST client bound to a specific organisation.
func NewClient(baseURL, token, org string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		org:     org,
		rest: restclient.New("sentry",
			restclient.WithHeader("Authorization", "Bearer "+token),
			restclient.WithHeader("Accept", "application/json"),
			restclient.WithStatusHint(http.StatusUnauthorized,
				"sentry auth failed (HTTP 401) — check SENTRY_AUTH_TOKEN"),
			restclient.WithStatusHint(http.StatusForbidden,
				"sentry auth failed (HTTP 403) — check SENTRY_AUTH_TOKEN"),
			restclient.WithStatusHint(http.StatusNotFound,
				"sentry resource not found (HTTP 404) — check the project, issue ID or release version"),
		),
	}
}

// ── Sentry JSON model (minimal projection of what the tools need) ───────────

// Issue is a deduplicated group of similar events ("a Sentry issue").
type Issue struct {
	ID           string    `json:"id"`
	ShortID      string    `json:"shortId"`
	Title        string    `json:"title"`
	Culprit      string    `json:"culprit"`
	Level        string    `json:"level"`
	Status       string    `json:"status"`
	Permalink    string    `json:"permalink"`
	Count        string    `json:"count"`
	UserCount    int       `json:"userCount"`
	FirstSeen    time.Time `json:"firstSeen"`
	LastSeen     time.Time `json:"lastSeen"`
	Project      Project   `json:"project"`
	Type         string    `json:"type"`
	Metadata     Metadata  `json:"metadata"`
	FirstRelease *Release  `json:"firstRelease,omitempty"`
}

// Project is the Sentry project an issue belongs to.
type Project struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Metadata is Sentry's compact failure summary attached to an issue
// (exception type, value, filename, function).
type Metadata struct {
	Type     string `json:"type"`
	Value    string `json:"value"`
	Filename string `json:"filename"`
	Function string `json:"function"`
}

// Release is the deploy version that a Sentry issue is attributed to.
type Release struct {
	Version string    `json:"version"`
	Created time.Time `json:"dateCreated"`
}

// ── API methods ─────────────────────────────────────────────────────────────

// ListProjectIssues returns recent issues for a project, ordered by
// last-seen. Filter is a Sentry search query (e.g. "is:unresolved
// level:error"); pass "" for the default ("is:unresolved").
func (c *Client) ListProjectIssues(ctx context.Context, project, filter string, limit int) ([]Issue, error) {
	if filter == "" {
		filter = "is:unresolved"
	}
	params := url.Values{
		"query": {filter},
		"sort":  {"last_seen"},
	}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}

	path := fmt.Sprintf("/api/0/projects/%s/%s/issues/",
		url.PathEscape(c.org), url.PathEscape(project))
	var issues []Issue
	if err := c.doJSON(ctx, path, params, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// GetIssue returns the full detail of a single issue by numeric ID.
func (c *Client) GetIssue(ctx context.Context, issueID string) (*Issue, error) {
	path := fmt.Sprintf("/api/0/issues/%s/", url.PathEscape(issueID))
	var issue Issue
	if err := c.doJSON(ctx, path, nil, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// ListReleaseIssues returns the unresolved issues first seen in a specific
// release — Sentry's "what did this release break" view.
func (c *Client) ListReleaseIssues(ctx context.Context, version string, limit int) ([]Issue, error) {
	params := url.Values{
		"query": {fmt.Sprintf("firstRelease:%s is:unresolved", version)},
		"sort":  {"last_seen"},
	}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := fmt.Sprintf("/api/0/organizations/%s/issues/", url.PathEscape(c.org))
	var issues []Issue
	if err := c.doJSON(ctx, path, params, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

func (c *Client) doJSON(ctx context.Context, path string, params url.Values, dst any) error {
	return c.rest.GetJSON(ctx, restclient.JoinURL(c.baseURL, path, params), dst)
}
