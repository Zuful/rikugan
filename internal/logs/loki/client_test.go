package loki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_QueryRange_HappyPath(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "streams",
				"result": [
					{
						"stream": {"app": "payments", "level": "ERROR"},
						"values": [
							["1747930338000000000", "ERROR: connection refused"],
							["1747930330000000000", "ERROR: retry exhausted"]
						]
					}
				]
			}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	streams, err := c.QueryRange(context.Background(), `{app="payments"}`, "1747930000000000000", "1747930500000000000", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("got %d streams, want 1", len(streams))
	}
	if len(streams[0].Values) != 2 {
		t.Errorf("expected 2 log entries, got %d", len(streams[0].Values))
	}
	if streams[0].Stream["app"] != "payments" {
		t.Errorf("missing app label, got %v", streams[0].Stream)
	}

	// Query params propagate.
	for _, want := range []string{`query=%7Bapp%3D%22payments%22%7D`, "start=1747930000000000000", "end=1747930500000000000", "limit=100", "direction=backward"} {
		if !strings.Contains(capturedURL, want) {
			t.Errorf("expected %q in URL, got %s", want, capturedURL)
		}
	}
}

func TestClient_QueryRange_LokiErrorStatusRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Valid HTTP 200 but the Loki-level "status" field is "error".
		_, _ = w.Write([]byte(`{"status":"error","data":{"resultType":"streams","result":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.QueryRange(context.Background(), `{app="x"}`, "0", "1", 100)
	if err == nil {
		t.Fatal("expected error on non-success Loki status, got nil")
	}
	if !strings.Contains(err.Error(), "loki query returned status: error") {
		t.Errorf("error %q should mention the Loki-level status", err)
	}
}

func TestClient_doJSON_HTTPErrorStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("loki internal failure"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.QueryRange(context.Background(), `{app="x"}`, "0", "1", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should include status code", err)
	}
	if !strings.Contains(err.Error(), "loki internal failure") {
		t.Errorf("error %q should surface the body", err)
	}
}

func TestClient_doJSON_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.QueryRange(context.Background(), `{app="x"}`, "0", "1", 1)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode loki response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

func TestToNano(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"unix zero", time.Unix(0, 0), "0"},
		{"nanosecond precision preserved", time.Unix(0, 1747930338123456789), "1747930338123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ToNano(tt.in); got != tt.want {
				t.Errorf("ToNano(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
