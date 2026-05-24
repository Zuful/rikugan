package falco

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_ListEvents_HappyPath(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[
			{"time":"2026-05-22T14:00:00Z","rule":"Terminal shell in container","priority":"Warning","output":"shell opened","source":"syscall","output_fields":{"k8s.ns.name":"prod","k8s.pod.name":"api-7"}},
			{"time":"2026-05-22T14:05:00Z","rule":"Write below etc","priority":"Critical","output":"etc write","source":"syscall","output_fields":{"k8s.ns.name":"prod"}}
		]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	events, err := c.ListEvents(context.Background(), "Warning", 30*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	// Filters survive the round-trip into the query string.
	if !strings.Contains(capturedURL, "priority=Warning") {
		t.Errorf("expected priority filter in URL, got %s", capturedURL)
	}
	if !strings.Contains(capturedURL, "since=1800s") {
		t.Errorf("expected since=1800s (30m) in URL, got %s", capturedURL)
	}

	// Field helper unpacks output_fields strings.
	if got := events[0].FieldString("k8s.ns.name"); got != "prod" {
		t.Errorf("FieldString = %q, want prod", got)
	}
	if got := events[0].FieldString("does.not.exist"); got != "" {
		t.Errorf("missing key should return empty string, got %q", got)
	}
}

func TestClient_ListEvents_OmitsEmptyFilters(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.ListEvents(context.Background(), "", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(capturedURL, "priority=") {
		t.Errorf("empty priority should be omitted, got %s", capturedURL)
	}
	if strings.Contains(capturedURL, "since=") {
		t.Errorf("zero since should be omitted, got %s", capturedURL)
	}
}

func TestClient_doJSON_ErrorStatuses(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantSubstr string
	}{
		{"500 surfaces body", http.StatusInternalServerError, "kaboom", "500"},
		{"404 surfaces body", http.StatusNotFound, "missing", "404"},
		{"401 surfaces body", http.StatusUnauthorized, "no token", "401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			_, err := c.ListEvents(context.Background(), "", 0)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error %q should contain %q", err, tt.wantSubstr)
			}
			if !strings.Contains(err.Error(), tt.body) {
				t.Errorf("error %q should surface response body %q", err, tt.body)
			}
		})
	}
}

func TestClient_doJSON_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.ListEvents(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode falcosidekick response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

func TestClient_doJSON_ContextCancellation(t *testing.T) {
	// Server that blocks until the request context is cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.ListEvents(ctx, "", 0)
	if err == nil {
		t.Fatal("expected context-cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "falcosidekick request failed") {
		t.Errorf("error %q should be wrapped with the request-failure prefix", err)
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://falco.example.com/")
	if c.baseURL != "https://falco.example.com" {
		t.Errorf("trailing slash should be trimmed, got %q", c.baseURL)
	}
}
