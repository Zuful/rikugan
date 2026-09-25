package jaeger

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Zuful/rikugan/pkg/restclient"
)

// Client is a thin wrapper around the Jaeger Query REST API.
type Client struct {
	baseURL string
	rest    *restclient.Client
}

// NewClient creates a Jaeger REST client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		rest:    restclient.New("jaeger"),
	}
}

// ── Jaeger JSON model (minimal) ─────────────────────────────────────────────

// Trace represents a single trace containing spans and process metadata.
type Trace struct {
	TraceID   string             `json:"traceID"`
	Spans     []Span             `json:"spans"`
	Processes map[string]Process `json:"processes"`
}

// Span represents a single span within a trace.
type Span struct {
	TraceID       string    `json:"traceID"`
	SpanID        string    `json:"spanID"`
	OperationName string    `json:"operationName"`
	ProcessID     string    `json:"processID"`
	StartTime     int64     `json:"startTime"` // microseconds since epoch
	Duration      int64     `json:"duration"`  // microseconds
	Tags          []Tag     `json:"tags"`
	Logs          []SpanLog `json:"logs"`
}

// Tag is a key-value pair attached to a span.
type Tag struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// SpanLog is a timestamped log entry within a span.
type SpanLog struct {
	Timestamp int64 `json:"timestamp"`
	Fields    []Tag `json:"fields"`
}

// Process identifies the service that emitted a set of spans.
type Process struct {
	ServiceName string `json:"serviceName"`
}

type traceResponse struct {
	Data []Trace `json:"data"`
}

// ── API methods ─────────────────────────────────────────────────────────────

// FindTraces queries for traces matching the given service and optional
// minimum duration. Returns up to limit traces, ordered by most recent.
func (c *Client) FindTraces(ctx context.Context, service string, limit int, minDuration string) ([]Trace, error) {
	params := url.Values{}
	params.Set("service", service)
	params.Set("limit", fmt.Sprintf("%d", limit))
	// Jaeger expects lookback as a duration string or explicit start/end.
	// Use a 24h window by default for recency.
	params.Set("lookback", "24h")
	if minDuration != "" {
		params.Set("minDuration", minDuration)
	}

	u := fmt.Sprintf("%s/api/traces?%s", c.baseURL, params.Encode())

	var resp traceResponse
	if err := c.rest.GetJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// GetTrace fetches a single trace by ID.
func (c *Client) GetTrace(ctx context.Context, traceID string) (*Trace, error) {
	u := fmt.Sprintf("%s/api/traces/%s", c.baseURL, traceID)

	var resp traceResponse
	if err := c.rest.GetJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("trace %s not found", traceID)
	}
	return &resp.Data[0], nil
}
