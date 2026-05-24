package sentry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_doJSON_SendsBearerAuthAndJSONAccept(t *testing.T) {
	var capturedAuth, capturedAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok-123", "my-org")
	if _, err := c.ListProjectIssues(context.Background(), "web", "", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", capturedAuth, "Bearer tok-123")
	}
	if capturedAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", capturedAccept)
	}
}

func TestClient_ListProjectIssues_DefaultFilterAndPath(t *testing.T) {
	var capturedPath, capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"id":"123","shortId":"WEB-A12","title":"crash","level":"error","count":"7"}
		]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "my-org")
	issues, err := c.ListProjectIssues(context.Background(), "web", "", 25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0].ShortID != "WEB-A12" {
		t.Fatalf("unexpected decode: %+v", issues)
	}
	// Path embeds org + project.
	if capturedPath != "/api/0/projects/my-org/web/issues/" {
		t.Errorf("path = %q, want /api/0/projects/my-org/web/issues/", capturedPath)
	}
	// Empty filter defaults to "is:unresolved".
	if !strings.Contains(capturedQuery, "query=is%3Aunresolved") {
		t.Errorf("expected default query is:unresolved in URL, got %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "limit=25") {
		t.Errorf("expected limit=25 in URL, got %s", capturedQuery)
	}
}

func TestClient_ListProjectIssues_ExplicitFilterEchoed(t *testing.T) {
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "org")
	if _, err := c.ListProjectIssues(context.Background(), "web", "level:error is:unresolved", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(capturedQuery, "level%3Aerror") {
		t.Errorf("expected explicit filter to be passed through, got %s", capturedQuery)
	}
	if strings.Contains(capturedQuery, "limit=") {
		t.Errorf("limit should be omitted when zero, got %s", capturedQuery)
	}
}

func TestClient_GetIssue_PathEmbedsIssueID(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"987654321","shortId":"WEB-A12","title":"panic"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "org")
	issue, err := c.GetIssue(context.Background(), "987654321")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issue.ShortID != "WEB-A12" {
		t.Errorf("ShortID = %q, want WEB-A12", issue.ShortID)
	}
	if capturedPath != "/api/0/issues/987654321/" {
		t.Errorf("path = %q, want /api/0/issues/987654321/", capturedPath)
	}
}

func TestClient_ListReleaseIssues_QueryEmbedsVersion(t *testing.T) {
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "org")
	if _, err := c.ListReleaseIssues(context.Background(), "v1.2.3", 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// firstRelease:v1.2.3 should round-trip into the query string.
	if !strings.Contains(capturedQuery, "firstRelease%3Av1.2.3") {
		t.Errorf("expected firstRelease:v1.2.3 in query, got %s", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "is%3Aunresolved") {
		t.Errorf("expected is:unresolved in query, got %s", capturedQuery)
	}
}

func TestClient_doJSON_AuthErrorsAreSpecialCased(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "bad-tok", "org")
			_, err := c.ListProjectIssues(context.Background(), "web", "", 0)
			if err == nil {
				t.Fatal("expected auth error, got nil")
			}
			if !strings.Contains(err.Error(), "sentry auth failed") {
				t.Errorf("error %q should mention auth failure", err)
			}
			if !strings.Contains(err.Error(), "SENTRY_AUTH_TOKEN") {
				t.Errorf("error %q should hint at the env var to set", err)
			}
		})
	}
}

func TestClient_doJSON_NotFoundIsSpecialCased(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "org")
	_, err := c.GetIssue(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "sentry resource not found") {
		t.Errorf("error %q should be the special-cased 404 message", err)
	}
}

func TestClient_doJSON_GenericErrorFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream is down"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "org")
	_, err := c.ListProjectIssues(context.Background(), "web", "", 0)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should include status code", err)
	}
	if !strings.Contains(err.Error(), "upstream is down") {
		t.Errorf("error %q should surface the body", err)
	}
}

func TestClient_doJSON_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "org")
	_, err := c.ListProjectIssues(context.Background(), "web", "", 0)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode sentry response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://sentry.io/", "tok", "org")
	if c.baseURL != "https://sentry.io" {
		t.Errorf("trailing slash should be trimmed, got %q", c.baseURL)
	}
}
