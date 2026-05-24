package argocd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_doJSON_SendsBearerToken(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "argo-token")
	if _, err := c.ListApps(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedAuth != "Bearer argo-token" {
		t.Errorf("Authorization = %q, want %q", capturedAuth, "Bearer argo-token")
	}
}

func TestClient_GetApp_HappyPath(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"metadata": {"name": "checkout", "namespace": "argocd"},
			"spec": {"project": "platform", "source": {"repoURL": "git@github.com:Zuful/checkout.git", "targetRevision": "main", "path": "k8s"}},
			"status": {
				"sync": {"status": "Synced", "revision": "abc123"},
				"health": {"status": "Healthy", "message": ""}
			}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	app, err := c.GetApp(context.Background(), "checkout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if app.Metadata.Name != "checkout" {
		t.Errorf("Name = %q, want checkout", app.Metadata.Name)
	}
	if app.Status.Sync.Status != "Synced" {
		t.Errorf("Sync.Status = %q, want Synced", app.Status.Sync.Status)
	}
	if app.Status.Health.Status != "Healthy" {
		t.Errorf("Health.Status = %q, want Healthy", app.Status.Health.Status)
	}
	if capturedPath != "/api/v1/applications/checkout" {
		t.Errorf("path = %q, want /api/v1/applications/checkout", capturedPath)
	}
}

func TestClient_ListApps_ProjectFilter(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"items": [
			{"metadata": {"name": "a"}},
			{"metadata": {"name": "b"}}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	apps, err := c.ListApps(context.Background(), "platform")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(apps) != 2 {
		t.Errorf("got %d apps, want 2", len(apps))
	}
	if !strings.Contains(capturedURL, "project=platform") {
		t.Errorf("expected project=platform in URL, got %s", capturedURL)
	}
}

func TestClient_ListApps_OmitsProjectFilterWhenEmpty(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"items": []}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	if _, err := c.ListApps(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(capturedURL, "project=") {
		t.Errorf("project param should be omitted when empty, got %s", capturedURL)
	}
}

func TestClient_doJSON_ErrorStatusSurfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("permission denied"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	_, err := c.GetApp(context.Background(), "checkout")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should include status", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q should surface body", err)
	}
}

func TestClient_doJSON_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	_, err := c.GetApp(context.Background(), "checkout")
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode argocd response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://argo.example.com/", "token")
	if c.baseURL != "https://argo.example.com" {
		t.Errorf("trailing slash should be trimmed, got %q", c.baseURL)
	}
}
