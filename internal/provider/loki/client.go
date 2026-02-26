package loki

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

// Client is a thin wrapper around the Loki HTTP API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Loki REST client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: clientTimeout,
		},
	}
}

// ── Loki JSON model ─────────────────────────────────────────────────────────

// queryResponse is the top-level Loki query_range response.
type queryResponse struct {
	Status string    `json:"status"`
	Data   queryData `json:"data"`
}

// queryData holds the result type and the stream array.
type queryData struct {
	ResultType string   `json:"resultType"`
	Result     []stream `json:"result"`
}

// stream represents a single log stream with its label set and log entries.
type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"` // each entry: [nanosecond-timestamp, log-line]
}

// ── API methods ─────────────────────────────────────────────────────────────

// QueryRange fetches log entries matching a LogQL query within the given time range.
// Start and end are nanosecond-epoch strings. Results are returned in backward
// (newest-first) order.
func (c *Client) QueryRange(ctx context.Context, query, start, end string, limit int) ([]stream, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", start)
	params.Set("end", end)
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("direction", "backward")

	u := fmt.Sprintf("%s/loki/api/v1/query_range?%s", c.baseURL, params.Encode())

	var resp queryResponse
	if err := c.doJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("loki query returned status: %s", resp.Status)
	}
	return resp.Data.Result, nil
}

func (c *Client) doJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("loki request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("loki returned %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode loki response: %w", err)
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// ToNano converts a time.Time to the nanosecond-epoch string Loki expects.
func ToNano(t time.Time) string {
	return fmt.Sprintf("%d", t.UnixNano())
}
