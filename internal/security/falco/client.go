package falco

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Zuful/rikugan/pkg/restclient"
)

// Client is a thin wrapper around the Falcosidekick REST API.
type Client struct {
	baseURL string
	rest    *restclient.Client
}

// NewClient creates a Falcosidekick REST client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		rest:    restclient.New("falcosidekick"),
	}
}

// ── Falcosidekick JSON model (minimal) ──────────────────────────────────────

// Event represents a single Falco security event.
type Event struct {
	Time         time.Time      `json:"time"`
	Rule         string         `json:"rule"`
	Priority     string         `json:"priority"`
	Output       string         `json:"output"`
	Source       string         `json:"source"`
	OutputFields map[string]any `json:"output_fields"`
	Tags         []string       `json:"tags"`
}

// ── API methods ─────────────────────────────────────────────────────────────

// ListEvents fetches recent events, optionally filtered by minimum priority.
// The since parameter limits events to the given lookback window.
func (c *Client) ListEvents(ctx context.Context, priority string, since time.Duration) ([]Event, error) {
	params := url.Values{}
	if priority != "" {
		params.Set("priority", priority)
	}
	if since > 0 {
		params.Set("since", fmt.Sprintf("%ds", int(since.Seconds())))
	}

	u := fmt.Sprintf("%s/api/v1/events", c.baseURL)
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	var events []Event
	if err := c.rest.GetJSON(ctx, u, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// ── Field helpers ───────────────────────────────────────────────────────────

// FieldString extracts a string field from output_fields.
func (e *Event) FieldString(key string) string {
	if v, ok := e.OutputFields[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
