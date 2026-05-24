package jaeger

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_FindTraces_HappyPath(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"traceID": "abc123",
					"spans": [{"traceID":"abc123","spanID":"s1","operationName":"GET /api","startTime":1700000000000000,"duration":250000}],
					"processes": {"p1": {"serviceName": "checkout"}}
				}
			]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	traces, err := c.FindTraces(context.Background(), "checkout", 50, "500ms")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if traces[0].TraceID != "abc123" {
		t.Errorf("TraceID = %q, want abc123", traces[0].TraceID)
	}

	// Required params propagate to URL.
	for _, want := range []string{"service=checkout", "limit=50", "lookback=24h", "minDuration=500ms"} {
		if !strings.Contains(capturedURL, want) {
			t.Errorf("expected %q in URL, got %s", want, capturedURL)
		}
	}
}

func TestClient_FindTraces_OmitsMinDurationWhenEmpty(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.FindTraces(context.Background(), "checkout", 50, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(capturedURL, "minDuration=") {
		t.Errorf("empty minDuration should be omitted, got %s", capturedURL)
	}
}

func TestClient_GetTrace_HappyPath(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"traceID":"xyz789","spans":[],"processes":{}}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	tr, err := c.GetTrace(context.Background(), "xyz789")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.TraceID != "xyz789" {
		t.Errorf("TraceID = %q, want xyz789", tr.TraceID)
	}
	if capturedPath != "/api/traces/xyz789" {
		t.Errorf("path = %q, want /api/traces/xyz789", capturedPath)
	}
}

func TestClient_GetTrace_EmptyDataReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.GetTrace(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "trace missing not found") {
		t.Errorf("error %q should mention not found", err)
	}
}

func TestClient_doJSON_ErrorStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusInternalServerError,
	} {
		status := status
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("boom"))
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			_, err := c.FindTraces(context.Background(), "any", 1, "")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", status)) {
				t.Errorf("error %q should contain status code", err)
			}
		})
	}
}

func TestClient_doJSON_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.FindTraces(context.Background(), "x", 1, "")
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode jaeger response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}
