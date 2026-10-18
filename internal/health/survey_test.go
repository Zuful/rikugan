package health

import (
	"context"
	"testing"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

func minuteSeries(perMinuteCounts []float64) Series {
	var s Series
	for i, n := range perMinuteCounts {
		s = append(s, provider.MetricSample{Time: completed.Add(time.Duration(i) * time.Minute), Value: perMinute(n)})
	}
	return s
}

func repeat(n float64, times int) []float64 {
	out := make([]float64, times)
	for i := range out {
		out[i] = n
	}
	return out
}

func TestInsufficientShare(t *testing.T) {
	// Medium traffic, 10 req/min all along: a 5- or 15-minute window
	// never reaches 200 requests, a 30-minute window always does.
	medium := minuteSeries(repeat(10, 120))
	for _, tc := range []struct {
		window time.Duration
		want   float64
	}{
		{5 * time.Minute, 1}, {15 * time.Minute, 1}, {30 * time.Minute, 0},
	} {
		if got := insufficientShare(medium, tc.window, 200); got != tc.want {
			t.Errorf("medium traffic, %s windows: %.2f, want %.2f", tc.window, got, tc.want)
		}
	}

	// Busy by day (100 req/min for 60 minutes), silent by night (60
	// minutes without samples): 5-minute windows are short half the
	// time, give or take the windows straddling the edge.
	dayNight := minuteSeries(append(repeat(100, 60), repeat(0, 60)...))
	dayNight = dayNight[:60]                                                                             // night minutes have no sample at all
	dayNight = append(dayNight, provider.MetricSample{Time: completed.Add(119 * time.Minute), Value: 0}) // series ends at the end of the night
	got := insufficientShare(dayNight, 5*time.Minute, 200)
	// 116 windows. The 56 fully in the day pass, and so do the 3
	// straddling ones holding 4, 3 or 2 day minutes (2 × 100 = 200 is
	// not below the 200 minimum) → 57 below.
	if want := 57.0 / 116; got != want {
		t.Errorf("day/night, 5-minute windows: %.3f, want %.3f", got, want)
	}

	if got := insufficientShare(nil, 5*time.Minute, 200); got != 1 {
		t.Errorf("no data: %.2f, want 1", got)
	}
}

func TestSurvey(t *testing.T) {
	m := &fakeMetrics{values: []metricValue{{"http_requests_total", func(time.Time) float64 { return perMinute(10) }}}}
	res, err := Survey(context.Background(), m, DefaultConfig(), DefaultTemplates(), svcAPI, completed, completed.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	if res.Minutes != 121 || res.Insufficient[5*time.Minute] != 1 || res.Insufficient[30*time.Minute] != 0 {
		t.Errorf("got %+v", res)
	}
	if len(m.calls) != 1 {
		t.Errorf("want one range query, got %d", len(m.calls))
	}
}
