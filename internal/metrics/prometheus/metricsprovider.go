package prometheus

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.MetricsProvider —
// the concept-level Metrics tools (get_metric / compare_to_baseline /
// list_available_metrics) can route to this backend.
var _ provider.MetricsProvider = (*Provider)(nil)

// GetMetric runs a PromQL range query and returns the result as a flat
// list of provider.MetricSample. One sample is emitted per (series,
// data-point) pair so the caller can re-group however it likes.
func (p *Provider) GetMetric(
	ctx context.Context,
	query string,
	start, end time.Time,
	step time.Duration,
) ([]provider.MetricSample, error) {
	if step <= 0 {
		step = 60 * time.Second
	}
	result, _, _, err := p.api.QueryRange(ctx, query, promv1.Range{
		Start: start,
		End:   end,
		Step:  step,
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus query_range: %w", err)
	}
	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T; expected matrix", result)
	}
	return matrixToSamples(matrix), nil
}

// CompareToBaseline runs the same PromQL for the most-recent `window`
// and for a baseline `window` ending `baseline` ago, and reports the
// mean of each side plus the delta. The shape works for any single-
// value query (e.g. an aggregated rate). Multi-series queries are
// summarised by their global mean — the markdown report lists each
// series's mean separately as a sanity check.
func (p *Provider) CompareToBaseline(
	ctx context.Context,
	query string,
	window, baseline time.Duration,
) (string, error) {
	if window <= 0 {
		return "", fmt.Errorf("window must be positive")
	}
	if baseline <= 0 {
		return "", fmt.Errorf("baseline must be positive")
	}
	now := time.Now()
	step := pickStep(window)

	currStart, currEnd := now.Add(-window), now
	baseStart, baseEnd := now.Add(-baseline-window), now.Add(-baseline)

	curr, err := p.GetMetric(ctx, query, currStart, currEnd, step)
	if err != nil {
		return "", fmt.Errorf("current window: %w", err)
	}
	base, err := p.GetMetric(ctx, query, baseStart, baseEnd, step)
	if err != nil {
		return "", fmt.Errorf("baseline window: %w", err)
	}

	return buildBaselineReport(query, window, baseline, currStart, currEnd, baseStart, baseEnd, curr, base), nil
}

// ListAvailableMetrics returns the set of metric names known to the
// server, optionally filtered by a prefix. The list is sourced from
// the __name__ label values endpoint and is sorted alphabetically.
// matchPrefix is matched case-sensitively against the start of each
// metric name; an empty prefix returns the full list.
func (p *Provider) ListAvailableMetrics(
	ctx context.Context,
	matchPrefix string,
) ([]string, error) {
	// Limit the lookup window to the last 24h to avoid heavy server
	// scans; metric names typically stable across that window.
	end := time.Now()
	start := end.Add(-24 * time.Hour)
	values, _, _, err := p.api.LabelValues(ctx, "__name__", nil, start, end)
	if err != nil {
		return nil, fmt.Errorf("prometheus LabelValues(__name__): %w", err)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		s := string(v)
		if matchPrefix == "" || strings.HasPrefix(s, matchPrefix) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func matrixToSamples(matrix model.Matrix) []provider.MetricSample {
	var out []provider.MetricSample
	for _, series := range matrix {
		labels := make(map[string]string, len(series.Metric))
		for k, v := range series.Metric {
			labels[string(k)] = string(v)
		}
		for _, sp := range series.Values {
			out = append(out, provider.MetricSample{
				Time:   sp.Timestamp.Time(),
				Value:  float64(sp.Value),
				Labels: labels,
			})
		}
	}
	return out
}

// pickStep keeps total points under ~600 to bound report size.
func pickStep(window time.Duration) time.Duration {
	step := window / 600
	if step < time.Second {
		step = time.Second
	}
	// Round to nearest 5s for stable bucketing.
	const bucket = 5 * time.Second
	return ((step + bucket/2) / bucket) * bucket
}

// mean computes the arithmetic mean of the sample values; returns NaN
// when no samples are present so the report can show "n/a".
func mean(samples []provider.MetricSample) float64 {
	if len(samples) == 0 {
		return nanValue()
	}
	var sum float64
	for _, s := range samples {
		sum += s.Value
	}
	return sum / float64(len(samples))
}

func nanValue() float64 {
	z := 0.0
	return z / z
}

func isNaN(f float64) bool { return f != f }

// buildBaselineReport renders the comparison as a markdown table.
func buildBaselineReport(
	query string,
	window, baseline time.Duration,
	currStart, currEnd, baseStart, baseEnd time.Time,
	curr, base []provider.MetricSample,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Compare to Baseline\n\n")
	fmt.Fprintf(&b, "```promql\n%s\n```\n\n", query)
	fmt.Fprintf(&b, "| Window | Range | Samples | Mean |\n")
	fmt.Fprintf(&b, "|--------|-------|---------|------|\n")
	fmt.Fprintf(&b, "| Current (%s) | %s → %s | %d | %s |\n",
		window, currStart.UTC().Format(time.RFC3339), currEnd.UTC().Format(time.RFC3339), len(curr), fmtVal(mean(curr)))
	fmt.Fprintf(&b, "| Baseline (-%s) | %s → %s | %d | %s |\n",
		baseline, baseStart.UTC().Format(time.RFC3339), baseEnd.UTC().Format(time.RFC3339), len(base), fmtVal(mean(base)))
	b.WriteString("\n")

	cm, bm := mean(curr), mean(base)
	if !isNaN(cm) && !isNaN(bm) && bm != 0 {
		delta := cm - bm
		pct := (delta / bm) * 100
		fmt.Fprintf(&b, "**Delta:** %+.3f (%+.1f%% vs baseline)\n", delta, pct)
	} else {
		b.WriteString("**Delta:** _baseline has no samples or is zero — cannot compute ratio._\n")
	}
	return b.String()
}

func fmtVal(v float64) string {
	if isNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", v)
}
