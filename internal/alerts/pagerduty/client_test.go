package pagerduty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// withTestServer points baseURL at a test server for the duration of t.
func withTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	original := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = original })

	return srv
}

func TestClient_doJSON_SendsAuthAndAcceptHeaders(t *testing.T) {
	var capturedAuth, capturedAccept string
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"incidents":[]}`))
	})

	c := NewClient("test-token-abc123")
	if _, err := c.ListIncidents(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedAuth != "Token token=test-token-abc123" {
		t.Errorf("Authorization = %q, want %q", capturedAuth, "Token token=test-token-abc123")
	}
	if capturedAccept != "application/vnd.pagerduty+json;version=2" {
		t.Errorf("Accept = %q, want PagerDuty v2 vnd content type", capturedAccept)
	}
}

func TestClient_ListIncidents_PassesParamsAndDecodesResponse(t *testing.T) {
	var capturedURL string
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"incidents":[
			{"id":"P1","incident_number":42,"title":"DB down","status":"triggered","urgency":"high"},
			{"id":"P2","incident_number":43,"title":"Cache miss","status":"acknowledged","urgency":"low"}
		]}`))
	})

	c := NewClient("token")
	params := url.Values{
		"statuses[]":    {"triggered", "acknowledged"},
		"service_ids[]": {"svcXYZ"},
	}
	incidents, err := c.ListIncidents(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(incidents) != 2 {
		t.Fatalf("got %d incidents, want 2", len(incidents))
	}
	if incidents[0].IncidentNumber != 42 || incidents[0].Title != "DB down" {
		t.Errorf("decoding broke for incident[0] = %+v", incidents[0])
	}
	// Multi-value query params should round-trip.
	for _, want := range []string{"statuses%5B%5D=triggered", "statuses%5B%5D=acknowledged", "service_ids%5B%5D=svcXYZ"} {
		if !strings.Contains(capturedURL, want) {
			t.Errorf("expected %q in URL, got %s", want, capturedURL)
		}
	}
}

func TestClient_FindServiceByName_OnlyExactMatchWins(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Echo the substring matches PagerDuty would return for a "checkout"
		// query — including near-misses we should NOT return.
		_, _ = w.Write([]byte(`{"services":[
			{"id":"S1","name":"checkout-staging"},
			{"id":"S2","name":"Checkout"},
			{"id":"S3","name":"checkout-canary"}
		]}`))
	})

	c := NewClient("token")
	svc, err := c.FindServiceByName(context.Background(), "checkout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Case-insensitive equality so "Checkout" wins.
	if svc.ID != "S2" {
		t.Errorf("expected exact-match service S2, got %s (name=%q)", svc.ID, svc.Name)
	}
}

func TestClient_FindServiceByName_NoExactMatchSurfacesCount(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"services":[
			{"id":"S1","name":"checkout-staging"},
			{"id":"S3","name":"checkout-canary"}
		]}`))
	})

	c := NewClient("token")
	_, err := c.FindServiceByName(context.Background(), "checkout")
	if err == nil {
		t.Fatal("expected error when no exact match, got nil")
	}
	if !strings.Contains(err.Error(), "no service exactly named") {
		t.Errorf("error %q should explain no exact match", err)
	}
	if !strings.Contains(err.Error(), "2 substring matches") {
		t.Errorf("error %q should report the substring-match count", err)
	}
}

func TestClient_doJSON_AuthErrorsAreSpecialCased(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			withTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			})

			c := NewClient("bad-token")
			_, err := c.ListIncidents(context.Background(), nil)
			if err == nil {
				t.Fatal("expected auth error, got nil")
			}
			if !strings.Contains(err.Error(), "pagerduty auth failed") {
				t.Errorf("error %q should mention auth failure", err)
			}
			if !strings.Contains(err.Error(), "PAGERDUTY_API_TOKEN") {
				t.Errorf("error %q should hint at the env var to set", err)
			}
		})
	}
}

func TestClient_doJSON_NotFoundIsSpecialCased(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})

	c := NewClient("token")
	_, err := c.ListIncidents(context.Background(), nil)
	if err == nil {
		t.Fatal("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "pagerduty resource not found") {
		t.Errorf("error %q should be the special-cased 404 message", err)
	}
}

func TestClient_doJSON_GenericErrorFallsThrough(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	})

	c := NewClient("token")
	_, err := c.ListIncidents(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should include status code", err)
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("error %q should surface the body", err)
	}
}

func TestClient_doJSON_MalformedJSON(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	})

	c := NewClient("token")
	_, err := c.ListIncidents(context.Background(), nil)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode pagerduty response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}
