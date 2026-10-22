package watch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

func TestCheckListen(t *testing.T) {
	tests := []struct {
		addr, token string
		ok          bool
	}{
		{"127.0.0.1:8080", "", true},
		{"[::1]:8080", "", true},
		{"localhost:8080", "", true},
		{"127.0.0.1:8080", "s3cret", true},
		{"0.0.0.0:8080", "", false},
		{":8080", "", false},
		{"10.0.0.5:8080", "", false},
		{"rikugan.internal:8080", "", false},
		{"0.0.0.0:8080", "s3cret", true},
		{":8080", "s3cret", true},
	}
	for _, tt := range tests {
		err := CheckListen(tt.addr, tt.token)
		if (err == nil) != tt.ok {
			t.Errorf("CheckListen(%q, token=%v) = %v", tt.addr, tt.token != "", err)
		}
	}
	err := CheckListen("0.0.0.0:8080", "")
	want := "refusing to listen on 0.0.0.0:8080 without RIKUGAN_SERVE_TOKEN: the verdicts API would be readable by anyone on the network. Set a token, or listen on 127.0.0.1"
	if err == nil || err.Error() != want {
		t.Errorf("message %q", err)
	}
}

// apiHarness records a few checks through the daemon, then serves them.
func apiHarness(t *testing.T, token string) (*harness, http.Handler) {
	h := newHarness(t)
	h.d.Checker = &verdictChecker{verdicts: map[time.Duration]health.Status{5 * time.Minute: health.StatusOK, 15 * time.Minute: health.StatusDegraded}}
	api := provider.Service{Name: "api", Namespace: "prod"}
	web := provider.Service{Name: "web", Namespace: "shop"}
	for _, svc := range []provider.Service{api, web} {
		r := provider.Rollout{Service: svc, Sequence: "4", State: provider.RolloutComplete, StartedAt: t0, CompletedAt: t0}
		h.source.current = r
		h.at(0).handle(r)
		h.at(5 * time.Minute).due()
		h.at(15 * time.Minute).due()
	}
	return h, NewAPI(h.d.Journal, token, h.clock.Now)
}

func get(t *testing.T, api http.Handler, url, token string) (*httptest.ResponseRecorder, VerdictsResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	var body VerdictsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec, body
}

func summary(vs []Verdict) string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Service+"@"+v.Checkpoint+"="+v.Verdict)
	}
	return strings.Join(out, " ")
}

func TestVerdictsAPI(t *testing.T) {
	_, api := apiHarness(t, "")

	_, all := get(t, api, "/v1/verdicts", "")
	// Newest first; both services were checked at the same instants, and
	// among equal times the last recorded comes first.
	if got := summary(all.Verdicts); got != "shop/web@15m0s=degraded prod/api@15m0s=degraded shop/web@5m0s=ok prod/api@5m0s=ok" {
		t.Errorf("all: %s", got)
	}
	if v := all.Verdicts[0]; v.Source != "rollout" || v.Revision != "4" || v.Outcome != "evaluated" {
		t.Errorf("fields: %+v", v)
	}

	_, one := get(t, api, "/v1/verdicts?service=prod/api", "")
	if got := summary(one.Verdicts); got != "prod/api@15m0s=degraded prod/api@5m0s=ok" {
		t.Errorf("by namespace/name: %s", got)
	}
	_, bare := get(t, api, "/v1/verdicts?service=web&limit=1", "")
	if got := summary(bare.Verdicts); got != "shop/web@15m0s=degraded" {
		t.Errorf("by bare name with a limit: %s", got)
	}
	// The clock is at +15 min: "since=5m" keeps the +15 checks only.
	_, recent := get(t, api, "/v1/verdicts?since=5m", "")
	if len(recent.Verdicts) != 2 {
		t.Errorf("since=5m: %s", summary(recent.Verdicts))
	}
	_, abs := get(t, api, "/v1/verdicts?since="+t0.Add(10*time.Minute).Format(time.RFC3339), "")
	if len(abs.Verdicts) != 2 {
		t.Errorf("since=<RFC 3339>: %s", summary(abs.Verdicts))
	}
	for _, bad := range []string{"?since=yesterday", "?limit=0", "?limit=x"} {
		if rec, _ := get(t, api, "/v1/verdicts"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", bad, rec.Code)
		}
	}
	if rec, _ := get(t, api, "/v1/verdicts", ""); rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", rec.Header().Get("Content-Type"))
	}
}

func TestVerdictsAPIToken(t *testing.T) {
	_, api := apiHarness(t, "s3cret")
	if rec, _ := get(t, api, "/v1/verdicts", ""); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("without token: %d", rec.Code)
	}
	if rec, _ := get(t, api, "/v1/verdicts", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", rec.Code)
	}
	if rec, body := get(t, api, "/v1/verdicts", "s3cret"); rec.Code != http.StatusOK || len(body.Verdicts) != 4 {
		t.Errorf("right token: %d, %d verdicts", rec.Code, len(body.Verdicts))
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/verdicts", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

// Ended investigations are listed apart from verdicts, as hypotheses;
// those still running are not.
func TestAPIListsInvestigations(t *testing.T) {
	h, api := apiHarness(t, "")
	inc := Incident{Key: "rollout/prod/api/4", Title: "prod/api: revision 4 degraded (+15m0s)",
		Services: []provider.Service{{Name: "api", Namespace: "prod"}}}
	for _, r := range []escalationRecord{
		{Event: EscalationStarted, Incident: inc},
		{Event: EscalationDone, Incident: inc, Session: "ses_1", Summary: "Probable cause: a nil map write."},
		{Event: EscalationStarted, Incident: Incident{Key: "x", Services: []provider.Service{{Name: "web", Namespace: "shop"}}}},
	} {
		if err := h.d.recordEscalation(r); err != nil {
			t.Fatal(err)
		}
	}
	_, body := get(t, api, "/v1/verdicts?service=prod/api", "")
	if len(body.Investigations) != 1 {
		t.Fatalf("investigations %+v, want the ended one", body.Investigations)
	}
	inv := body.Investigations[0]
	if inv.Outcome != EscalationDone || inv.Summary != "Probable cause: a nil map write." || inv.Session != "ses_1" ||
		inv.Trust != InvestigationTrust || inv.Services[0] != "prod/api" {
		t.Errorf("investigation %+v", inv)
	}
	if len(body.Verdicts) == 0 {
		t.Error("verdicts gone")
	}
	if _, body := get(t, api, "/v1/verdicts?service=shop/web", ""); len(body.Investigations) != 0 {
		t.Errorf("another service's investigation listed: %+v", body.Investigations)
	}
}
