package restclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGetJSON_DecodesSuccessfulResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"rikugan","count":6}`))
	}))
	defer srv.Close()

	var got struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := New("test").GetJSON(context.Background(), srv.URL, &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "rikugan" || got.Count != 6 {
		t.Errorf("decoded %+v, want {rikugan 6}", got)
	}
}

func TestGetJSON_SendsConfiguredHeaders(t *testing.T) {
	t.Parallel()
	var auth, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		accept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New("test",
		WithHeader("Authorization", "Token token=secret"),
		WithHeader("Accept", "application/vnd.example+json;version=2"),
	)
	if err := c.GetJSON(context.Background(), srv.URL, &struct{}{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != "Token token=secret" {
		t.Errorf("Authorization = %q, want the configured token scheme", auth)
	}
	if accept != "application/vnd.example+json;version=2" {
		t.Errorf("Accept = %q, want the configured content type", accept)
	}
}

func TestGetJSON_ErrorStatusSurfacesNameAndBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()

	err := New("loki").GetJSON(context.Background(), srv.URL, &struct{}{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	for _, want := range []string{"loki", "500", "upstream exploded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestGetJSON_StatusHintReplacesGenericError(t *testing.T) {
	t.Parallel()
	const hint = "sentry auth failed (HTTP 401) — check SENTRY_AUTH_TOKEN"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("raw upstream body nobody wants to read"))
	}))
	defer srv.Close()

	c := New("sentry", WithStatusHint(http.StatusUnauthorized, hint))
	err := c.GetJSON(context.Background(), srv.URL, &struct{}{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if err.Error() != hint {
		t.Errorf("error = %q, want exactly the registered hint", err)
	}
	if strings.Contains(err.Error(), "raw upstream body") {
		t.Error("a registered hint must not leak the upstream body")
	}
}

func TestGetJSON_UnhintedStatusStillFallsThrough(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))
	defer srv.Close()

	c := New("test", WithStatusHint(http.StatusNotFound, "not this one"))
	err := c.GetJSON(context.Background(), srv.URL, &struct{}{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "418") || !strings.Contains(err.Error(), "short and stout") {
		t.Errorf("error = %q, want the generic status+body form", err)
	}
}

func TestGetJSON_ErrorBodyIsTruncated(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()

	err := New("test").GetJSON(context.Background(), srv.URL, &struct{}{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if n := strings.Count(err.Error(), "x"); n != errorBodyLimit {
		t.Errorf("echoed %d body bytes, want the %d-byte cap", n, errorBodyLimit)
	}
}

func TestGetJSON_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"truncated":`))
	}))
	defer srv.Close()

	err := New("jaeger").GetJSON(context.Background(), srv.URL, &struct{}{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "decode jaeger response") {
		t.Errorf("error = %q, want it to name the decode step and the provider", err)
	}
}

func TestGetJSON_ContextCancellation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := New("test").GetJSON(ctx, srv.URL, &struct{}{})
	if err == nil {
		t.Fatal("expected a cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "test request failed") {
		t.Errorf("error = %q, want it wrapped with the provider label", err)
	}
}

func TestGetJSON_InvalidURL(t *testing.T) {
	t.Parallel()
	err := New("test").GetJSON(context.Background(), "://not a url", &struct{}{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "create request") {
		t.Errorf("error = %q, want the request-construction failure", err)
	}
}

func TestWithHTTPClient_NilIsIgnored(t *testing.T) {
	t.Parallel()
	c := New("test", WithHTTPClient(nil))
	if c.httpClient == nil {
		t.Fatal("a nil *http.Client must leave the default in place")
	}
	if c.httpClient.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want DefaultTimeout", c.httpClient.Timeout)
	}
}

func TestWithTimeout(t *testing.T) {
	t.Parallel()
	c := New("test", WithTimeout(5*time.Second))
	if c.httpClient.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", c.httpClient.Timeout)
	}
}

func TestJoinURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		base   string
		path   string
		params url.Values
		want   string
	}{
		{
			name: "no params",
			base: "https://api.example.com",
			path: "/incidents",
			want: "https://api.example.com/incidents",
		},
		{
			name: "trailing slash on base is dropped",
			base: "https://api.example.com/",
			path: "/incidents",
			want: "https://api.example.com/incidents",
		},
		{
			name:   "params are encoded",
			base:   "https://api.example.com",
			path:   "/incidents",
			params: url.Values{"statuses[]": {"triggered"}, "limit": {"25"}},
			want:   "https://api.example.com/incidents?limit=25&statuses%5B%5D=triggered",
		},
		{
			name:   "empty params add no question mark",
			base:   "https://api.example.com",
			path:   "/oncalls",
			params: url.Values{},
			want:   "https://api.example.com/oncalls",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := JoinURL(tt.base, tt.path, tt.params); got != tt.want {
				t.Errorf("JoinURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
