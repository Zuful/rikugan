package prometheus

import (
	"context"
	"strings"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// fakeAPI implements just enough of promv1.API to drive the
// MetricsProvider methods under test.
type fakeAPI struct {
	promv1.API
	lastQuery       string
	lastRange       promv1.Range
	queryResult     model.Value
	queryErr        error
	labelValuesCall struct {
		label   string
		matches []string
	}
	labelValuesRet model.LabelValues
	labelValuesErr error
}

func (f *fakeAPI) QueryRange(_ context.Context, q string, r promv1.Range, _ ...promv1.Option) (model.Value, promv1.Warnings, promv1.Infos, error) {
	f.lastQuery = q
	f.lastRange = r
	return f.queryResult, nil, nil, f.queryErr
}

func (f *fakeAPI) LabelValues(_ context.Context, label string, matches []string, _, _ time.Time, _ ...promv1.Option) (model.LabelValues, promv1.Warnings, promv1.Infos, error) {
	f.labelValuesCall.label = label
	f.labelValuesCall.matches = matches
	return f.labelValuesRet, nil, nil, f.labelValuesErr
}

func matrixWith(samples ...float64) model.Matrix {
	values := make([]model.SamplePair, len(samples))
	for i, v := range samples {
		values[i] = model.SamplePair{Timestamp: model.Time(int64(i) * 1000), Value: model.SampleValue(v)}
	}
	return model.Matrix{{
		Metric: model.Metric{"job": "api"},
		Values: values,
	}}
}

func TestProvider_GetMetric_FlattensMatrixToSamples(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{queryResult: matrixWith(1, 2, 3)}
	p := NewFromAPI(api)
	got, err := p.GetMetric(context.Background(), `up{job="api"}`, time.Unix(0, 0), time.Unix(10, 0), 5*time.Second)
	if err != nil {
		t.Fatalf("GetMetric: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("samples: got %d, want 3", len(got))
	}
	if got[0].Value != 1 || got[2].Value != 3 {
		t.Errorf("values mismatch: %v", got)
	}
	if got[0].Labels["job"] != "api" {
		t.Errorf("labels: got %v", got[0].Labels)
	}
	if api.lastQuery != `up{job="api"}` {
		t.Errorf("query: got %q", api.lastQuery)
	}
	if api.lastRange.Step != 5*time.Second {
		t.Errorf("step: got %v", api.lastRange.Step)
	}
}

func TestProvider_GetMetric_DefaultStepWhenNonPositive(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{queryResult: model.Matrix{}}
	p := NewFromAPI(api)
	if _, err := p.GetMetric(context.Background(), "x", time.Unix(0, 0), time.Unix(1, 0), 0); err != nil {
		t.Fatalf("GetMetric: %v", err)
	}
	if api.lastRange.Step != 60*time.Second {
		t.Errorf("default step: got %v, want 60s", api.lastRange.Step)
	}
}

func TestProvider_CompareToBaseline_ProducesDeltaReport(t *testing.T) {
	t.Parallel()
	// First call (current window) returns mean ~2, second (baseline) returns mean ~1.
	// The fake API returns the same response for both calls; we test the
	// markdown shape rather than the delta arithmetic precision.
	api := &fakeAPI{queryResult: matrixWith(2, 2, 2)}
	p := NewFromAPI(api)
	report, err := p.CompareToBaseline(context.Background(), "rate(x[1m])", 15*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatalf("CompareToBaseline: %v", err)
	}
	for _, want := range []string{
		"## Compare to Baseline",
		"```promql\nrate(x[1m])\n```",
		"| Current (15m0s) |",
		"| Baseline (-24h0m0s) |",
		"**Delta:**",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n----\n%s\n----", want, report)
		}
	}
}

func TestProvider_ListAvailableMetrics_FiltersByPrefix(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{labelValuesRet: model.LabelValues{
		"http_requests_total",
		"http_request_duration_seconds",
		"node_cpu_seconds",
		"go_goroutines",
	}}
	p := NewFromAPI(api)
	got, err := p.ListAvailableMetrics(context.Background(), "http_")
	if err != nil {
		t.Fatalf("ListAvailableMetrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("filtered: got %d (%v), want 2", len(got), got)
	}
	if got[0] != "http_request_duration_seconds" || got[1] != "http_requests_total" {
		t.Errorf("sort/filter mismatch: %v", got)
	}
	if api.labelValuesCall.label != "__name__" {
		t.Errorf("label: got %q, want __name__", api.labelValuesCall.label)
	}
}

func TestProvider_ListAvailableMetrics_NoPrefixReturnsAll(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{labelValuesRet: model.LabelValues{"z", "a", "m"}}
	p := NewFromAPI(api)
	got, err := p.ListAvailableMetrics(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAvailableMetrics: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "z" {
		t.Errorf("got %v, want sorted [a m z]", got)
	}
}
