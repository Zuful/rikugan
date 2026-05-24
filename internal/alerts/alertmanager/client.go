package alertmanager

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

// Client is a thin wrapper around the AlertManager v2 REST API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates an AlertManager REST client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: clientTimeout,
		},
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
	if err := c.doJSON(ctx, u, &alerts); err != nil {
		return nil, err
	}
	return alerts, nil
}

func (c *Client) doJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("alertmanager request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("alertmanager returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode alertmanager response: %w", err)
	}
	return nil
}
