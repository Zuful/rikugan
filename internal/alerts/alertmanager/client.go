package alertmanager

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/restclient"
)

// Client is a thin wrapper around the AlertManager v2 REST API.
type Client struct {
	baseURL string
	rest    *restclient.Client
}

// NewClient creates an AlertManager REST client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		rest:    restclient.New("alertmanager"),
	}
}

// ── AlertManager v2 JSON model (minimal projection) ──────────────────────────

// Alert is one currently-known alert in AlertManager.
type Alert struct {
	Fingerprint  string            `json:"fingerprint"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	Status       AlertStatus       `json:"status"`
}

// AlertStatus carries the runtime state of an alert.
type AlertStatus struct {
	State       string   `json:"state"` // active, suppressed, unprocessed
	SilencedBy  []string `json:"silencedBy"`
	InhibitedBy []string `json:"inhibitedBy"`
}

// ── API methods ─────────────────────────────────────────────────────────────

// ListAlerts returns every currently-known alert, optionally filtered
// by labels. Each label filter is passed verbatim to AlertManager as a
// repeated `filter=label="value"` query param.
func (c *Client) ListAlerts(ctx context.Context, labelFilters []string) ([]Alert, error) {
	u := c.baseURL + "/api/v2/alerts"
	if len(labelFilters) > 0 {
		params := url.Values{}
		for _, f := range labelFilters {
			params.Add("filter", f)
		}
		u += "?" + params.Encode()
	}

	var alerts []Alert
	if err := c.rest.GetJSON(ctx, u, &alerts); err != nil {
		return nil, err
	}
	return alerts, nil
}
