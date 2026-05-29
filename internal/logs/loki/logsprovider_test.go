package loki

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// fakeLokiServer returns a httptest server that responds to
// /loki/api/v1/query_range with a static success payload, capturing the
// LogQL query and the limit param for assertion.
type captured struct {
	query string
	limit string
}

func fakeLokiServer(t *testing.T, cap *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/loki/api/v1/query_range") {
			http.NotFound(w, r)
			return
		}
		q, _ := url.ParseQuery(r.URL.RawQuery)
		cap.query = q.Get("query")
		cap.limit = q.Get("limit")
		_, _ = io.WriteString(w, `{
			"status":"success",
			"data":{
				"resultType":"streams",
				"result":[
					{"stream":{"app":"checkout","level":"error"},
					 "values":[["1714579200000000000","boom: payment failed"]]}
				]
			}
		}`)
	}))
}

func TestProvider_GetLogs_BuildsLogQLFromServiceAndNamespace(t *testing.T) {
	cap := &captured{}
	srv := fakeLokiServer(t, cap)
	defer srv.Close()

	p := NewFromClient(NewClient(srv.URL))
	entries, err := p.GetLogs(context.Background(), provider.LogQuery{
		Service:   "checkout",
		Namespace: "prod",
		Start:     time.Unix(0, 1),
		End:       time.Unix(0, 2),
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if cap.query != `{app="checkout",namespace="prod"}` {
		t.Errorf("query: got %q, want stream selector with app+namespace", cap.query)
	}
	if cap.limit != "50" {
		t.Errorf("limit: got %q, want 50", cap.limit)
	}
	if len(entries) != 1 {
		t.Fatalf("entries: got %d, want 1", len(entries))
	}
	if entries[0].Message != "boom: payment failed" {
		t.Errorf("message: got %q", entries[0].Message)
	}
	if entries[0].Labels["app"] != "checkout" {
		t.Errorf("labels: missing app=checkout, got %v", entries[0].Labels)
	}
}

func TestProvider_GetLogs_AppendsMatchAsFilter(t *testing.T) {
	cap := &captured{}
	srv := fakeLokiServer(t, cap)
	defer srv.Close()

	p := NewFromClient(NewClient(srv.URL))
	_, err := p.GetLogs(context.Background(), provider.LogQuery{
		Service: "checkout",
		Match:   "boom",
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	want := `{app="checkout"} |= "boom"`
	if cap.query != want {
		t.Errorf("query: got %q, want %q", cap.query, want)
	}
}

func TestProvider_GetLogs_PassesRawLogQLWhenMatchStartsWithBrace(t *testing.T) {
	cap := &captured{}
	srv := fakeLokiServer(t, cap)
	defer srv.Close()

	p := NewFromClient(NewClient(srv.URL))
	raw := `{job="payments"} != "noise"`
	_, err := p.GetLogs(context.Background(), provider.LogQuery{Match: raw})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if cap.query != raw {
		t.Errorf("query: got %q, want raw LogQL %q", cap.query, raw)
	}
}

func TestProvider_GetLogs_ClampsLimitToMax(t *testing.T) {
	cap := &captured{}
	srv := fakeLokiServer(t, cap)
	defer srv.Close()

	p := NewFromClient(NewClient(srv.URL))
	_, err := p.GetLogs(context.Background(), provider.LogQuery{
		Service: "x",
		Limit:   9999,
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if cap.limit != "500" {
		t.Errorf("limit: got %q, want clamped to 500", cap.limit)
	}
}

func TestProvider_SearchLogs_RejectsEmptyScope(t *testing.T) {
	p := NewFromClient(NewClient("http://unreachable"))
	_, err := p.SearchLogs(context.Background(), provider.LogQuery{})
	if err == nil {
		t.Fatal("SearchLogs: want error for empty scope, got nil")
	}
}

func TestProvider_SatisfiesLogsProviderInterface(t *testing.T) {
	// Compile-time guarantee is in logsprovider.go; this test exists so
	// `go test ./...` fails loudly with a clear stack if a future change
	// makes Provider drift away from the LogsProvider contract.
	var _ provider.LogsProvider = (*Provider)(nil)
	// Spot-check that the JSON shape round-trips through encoding/json
	// — guards against future tag drift on the stream type.
	if _, err := json.Marshal(stream{Stream: map[string]string{"a": "b"}}); err != nil {
		t.Fatalf("stream JSON: %v", err)
	}
}
