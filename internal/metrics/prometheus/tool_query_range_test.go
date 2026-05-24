package prometheus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/common/model"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// ─── Mock API ────────────────────────────────────────────────────────────────

// mockAPI embeds promv1.API so we only need to override QueryRange.
type mockAPI struct {
	promv1.API
	result   model.Value
	warnings promv1.Warnings
	err      error
}

func (m *mockAPI) QueryRange(_ context.Context, _ string, _ promv1.Range, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return m.result, m.warnings, m.err
}

func newTestProvider(api promv1.API) *Provider {
	return &Provider{
		api:    api,
		logger: slog.Default(),
	}
}

func makeCallToolRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "query_range",
			Arguments: args,
		},
	}
}

// ─── Argument validation tests ───────────────────────────────────────────────

func TestHandleQueryRange_MissingQuery(t *testing.T) {
	p := newTestProvider(&mockAPI{})

	req := makeCallToolRequest(map[string]any{})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing query")
	}
}

func TestHandleQueryRange_InvalidStart(t *testing.T) {
	p := newTestProvider(&mockAPI{})

	req := makeCallToolRequest(map[string]any{
		"query": "up",
		"start": "not-a-time",
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for invalid start")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "invalid start") {
		t.Errorf("unexpected error message: %s", text)
	}
}

func TestHandleQueryRange_EndBeforeStart(t *testing.T) {
	p := newTestProvider(&mockAPI{})

	now := time.Now()
	req := makeCallToolRequest(map[string]any{
		"query": "up",
		"start": now.Format(time.RFC3339),
		"end":   now.Add(-1 * time.Hour).Format(time.RFC3339),
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result when end is before start")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "end must be after start") {
		t.Errorf("unexpected error message: %s", text)
	}
}

func TestHandleQueryRange_InvalidStep(t *testing.T) {
	p := newTestProvider(&mockAPI{})

	req := makeCallToolRequest(map[string]any{
		"query": "up",
		"step":  "not-a-duration",
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for invalid step")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "invalid step") {
		t.Errorf("unexpected error message: %s", text)
	}
}

func TestHandleQueryRange_TooManyDataPoints(t *testing.T) {
	p := newTestProvider(&mockAPI{})

	now := time.Now()
	req := makeCallToolRequest(map[string]any{
		"query": "up",
		"start": now.Add(-24 * time.Hour).Format(time.RFC3339),
		"end":   now.Format(time.RFC3339),
		"step":  "1s", // 86400 points > 10000
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for too many data points")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "max") {
		t.Errorf("unexpected error message: %s", text)
	}
}

// ─── End-to-end handler tests ────────────────────────────────────────────────

func TestHandleQueryRange_Success(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	matrix := model.Matrix{
		{
			Metric: model.Metric{"__name__": "up", "instance": "localhost:9090"},
			Values: []model.SamplePair{
				{Timestamp: model.TimeFromUnix(now.Add(-2 * time.Minute).Unix()), Value: 1},
				{Timestamp: model.TimeFromUnix(now.Add(-1 * time.Minute).Unix()), Value: 1},
				{Timestamp: model.TimeFromUnix(now.Unix()), Value: 0},
			},
		},
	}

	p := newTestProvider(&mockAPI{result: matrix})

	req := makeCallToolRequest(map[string]any{
		"query": "up",
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error")
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "## PromQL Range Query Results") {
		t.Error("missing report header")
	}
	if !strings.Contains(text, "up") {
		t.Error("missing query in report")
	}
	if !strings.Contains(text, "Series returned:** 1") {
		t.Error("missing series count")
	}
	if !strings.Contains(text, "localhost:9090") {
		t.Error("missing label in report")
	}
}

func TestHandleQueryRange_EmptyResult(t *testing.T) {
	p := newTestProvider(&mockAPI{result: model.Matrix{}})

	req := makeCallToolRequest(map[string]any{
		"query": "nonexistent_metric",
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error")
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "Series returned:** 0") {
		t.Error("expected 0 series in report")
	}
}

func TestHandleQueryRange_APIError(t *testing.T) {
	p := newTestProvider(&mockAPI{err: fmt.Errorf("connection refused")})

	req := makeCallToolRequest(map[string]any{
		"query": "up",
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for API failure")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "connection refused") {
		t.Errorf("expected API error in message, got: %s", text)
	}
}

func TestHandleQueryRange_WithWarnings(t *testing.T) {
	matrix := model.Matrix{
		{
			Metric: model.Metric{"__name__": "up"},
			Values: []model.SamplePair{
				{Timestamp: model.TimeFromUnix(time.Now().Unix()), Value: 1},
			},
		},
	}

	p := newTestProvider(&mockAPI{
		result:   matrix,
		warnings: promv1.Warnings{"some data may be incomplete"},
	})

	req := makeCallToolRequest(map[string]any{
		"query": "up",
	})

	result, err := p.handleQueryRange(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success")
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "### Warnings") {
		t.Error("missing warnings section")
	}
	if !strings.Contains(text, "some data may be incomplete") {
		t.Error("missing warning text")
	}
}

// ─── Report builder tests ────────────────────────────────────────────────────

func TestBuildQueryRangeReport_MultipleSeries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	matrix := model.Matrix{
		{
			Metric: model.Metric{"__name__": "cpu", "host": "a"},
			Values: []model.SamplePair{
				{Timestamp: model.TimeFromUnix(now.Unix()), Value: 0.5},
			},
		},
		{
			Metric: model.Metric{"__name__": "cpu", "host": "b"},
			Values: []model.SamplePair{
				{Timestamp: model.TimeFromUnix(now.Unix()), Value: 0.8},
			},
		},
	}

	report := buildQueryRangeReport("cpu", now.Add(-time.Hour), now, 60*time.Second, matrix, nil)

	if !strings.Contains(report, "### Series 1") {
		t.Error("missing series 1 header")
	}
	if !strings.Contains(report, "### Series 2") {
		t.Error("missing series 2 header")
	}
	if !strings.Contains(report, `host="a"`) {
		t.Error("missing host=a label")
	}
	if !strings.Contains(report, `host="b"`) {
		t.Error("missing host=b label")
	}
}

func TestBuildQueryRangeReport_DefaultsOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-defaultRange)
	step, _ := parseDuration(defaultStep)

	report := buildQueryRangeReport("up", start, now, step, model.Matrix{}, nil)

	if !strings.Contains(report, "## PromQL Range Query Results") {
		t.Error("missing header")
	}
	if !strings.Contains(report, "Series returned:** 0") {
		t.Error("expected 0 series")
	}
}

// ─── parseDuration tests ─────────────────────────────────────────────────────

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"60s", false},
		{"5m", false},
		{"1h", false},
		{"1d", false},   // Prometheus-style
		{"0s", true},    // zero
		{"-5s", true},   // negative
		{"bogus", true}, // invalid
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			_, err := parseDuration(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("parseDuration(%q): err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
		})
	}
}
