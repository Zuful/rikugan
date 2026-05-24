package sentry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const clientTimeout = 30 * time.Second

// Client is a thin wrapper around the Sentry REST API (api/0/). Each Client
// is scoped to one organisation — Sentry's project and release endpoints
// are org-relative, so binding it once at construction keeps call sites
// compact.
type Client struct {
	baseURL    string
	token      string
	org        string
	httpClient *http.Client
}

// NewClient creates a Sentry REST client bound to a specific organisation.
func NewClient(baseURL, token, org string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		org:     org,
		httpClient: &http.Client{
			Timeout: clientTimeout,
		},
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
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sentry request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("sentry auth failed (HTTP %d) — check SENTRY_AUTH_TOKEN", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("sentry resource not found (HTTP 404) — check the project, issue ID or release version")
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("sentry returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode sentry response: %w", err)
	}
	return nil
}
