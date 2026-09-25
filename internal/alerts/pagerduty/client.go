package pagerduty

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/restclient"
)

// baseURL is the PagerDuty API root. It's a var rather than a const so
// tests can swap it for an httptest.Server URL; production code never
// overrides it.
var baseURL = "https://api.pagerduty.com"

// Client is a thin wrapper around the PagerDuty REST API.
type Client struct {
	baseURL string
	rest    *restclient.Client
}

// NewClient creates a PagerDuty REST client. PagerDuty uses a custom
// auth scheme ("Token token=KEY"), not Bearer.
func NewClient(token string) *Client {
	return &Client{
		baseURL: baseURL,
		rest: restclient.New("pagerduty",
			restclient.WithHeader("Authorization", "Token token="+token),
			restclient.WithHeader("Accept", "application/vnd.pagerduty+json;version=2"),
			restclient.WithStatusHint(http.StatusUnauthorized,
				"pagerduty auth failed (HTTP 401) — check PAGERDUTY_API_TOKEN"),
			restclient.WithStatusHint(http.StatusForbidden,
				"pagerduty auth failed (HTTP 403) — check PAGERDUTY_API_TOKEN"),
			restclient.WithStatusHint(http.StatusNotFound,
				"pagerduty resource not found (HTTP 404) — check the service or policy ID"),
		),
	}
}

// ── PagerDuty JSON model (minimal projection of what the tools need) ────────

// Incident is a single PagerDuty incident.
type Incident struct {
	ID               string       `json:"id"`
	IncidentNumber   int          `json:"incident_number"`
	Title            string       `json:"title"`
	Description      string       `json:"description"`
	Status           string       `json:"status"`
	Urgency          string       `json:"urgency"`
	CreatedAt        time.Time    `json:"created_at"`
	LastStatusChange time.Time    `json:"last_status_change_at"`
	HTMLURL          string       `json:"html_url"`
	Service          Reference    `json:"service"`
	Assignments      []Assignment `json:"assignments"`
}

// Reference is the embedded summary PagerDuty returns when referencing
// another entity (service, user, escalation policy). Only Summary is
// guaranteed to be present.
type Reference struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Summary string `json:"summary"`
	HTMLURL string `json:"html_url"`
}

// Assignment ties an incident to the user currently expected to handle it.
type Assignment struct {
	At       time.Time `json:"at"`
	Assignee Reference `json:"assignee"`
}

// Service is a PagerDuty service (what triggers incidents).
type Service struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	HTMLURL          string    `json:"html_url"`
	EscalationPolicy Reference `json:"escalation_policy"`
}

// Oncall represents one current on-call assignment.
type Oncall struct {
	EscalationPolicy Reference  `json:"escalation_policy"`
	EscalationLevel  int        `json:"escalation_level"`
	Schedule         *Reference `json:"schedule,omitempty"`
	User             Reference  `json:"user"`
	Start            *time.Time `json:"start,omitempty"`
	End              *time.Time `json:"end,omitempty"`
}

// ── API methods ─────────────────────────────────────────────────────────────

// ListIncidents returns incidents filtered by the supplied params. Common
// filters: statuses[]=triggered, statuses[]=acknowledged, service_ids[]=X,
// since=RFC3339, until=RFC3339.
func (c *Client) ListIncidents(ctx context.Context, params url.Values) ([]Incident, error) {
	var resp struct {
		Incidents []Incident `json:"incidents"`
	}
	if err := c.doJSON(ctx, "/incidents", params, &resp); err != nil {
		return nil, err
	}
	return resp.Incidents, nil
}

// FindServiceByName looks up a service by exact name match. PagerDuty's
// /services endpoint supports a query parameter that does substring
// matching; this method keeps only exact-name hits to avoid surprising
// the caller.
func (c *Client) FindServiceByName(ctx context.Context, name string) (*Service, error) {
	params := url.Values{
		"query": {name},
		"limit": {"25"},
	}
	var resp struct {
		Services []Service `json:"services"`
	}
	if err := c.doJSON(ctx, "/services", params, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Services {
		if strings.EqualFold(resp.Services[i].Name, name) {
			return &resp.Services[i], nil
		}
	}
	return nil, fmt.Errorf("no service exactly named %q found (PagerDuty returned %d substring matches)", name, len(resp.Services))
}

// GetIncident returns the full detail of a single incident by ID.
func (c *Client) GetIncident(ctx context.Context, id string) (*Incident, error) {
	var resp struct {
		Incident Incident `json:"incident"`
	}
	if err := c.doJSON(ctx, "/incidents/"+url.PathEscape(id), nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Incident, nil
}

// ListOncalls returns current on-call assignments, optionally filtered by
// escalation_policy_ids[] or earliest=true (one per policy).
func (c *Client) ListOncalls(ctx context.Context, params url.Values) ([]Oncall, error) {
	var resp struct {
		Oncalls []Oncall `json:"oncalls"`
	}
	if err := c.doJSON(ctx, "/oncalls", params, &resp); err != nil {
		return nil, err
	}
	return resp.Oncalls, nil
}

func (c *Client) doJSON(ctx context.Context, path string, params url.Values, dst any) error {
	return c.rest.GetJSON(ctx, restclient.JoinURL(c.baseURL, path, params), dst)
}
