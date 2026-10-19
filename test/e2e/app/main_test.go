package main

import (
	"math"
	"net/http"
	"testing"
	"time"
)

// constant returns a random source that always draws v.
func constant(v float64) func() float64 { return func() float64 { return v } }

func zero(int) int { return 0 }

func TestOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		b     behaviour
		age   time.Duration
		draw  float64
		delay time.Duration
		code  int
	}{
		{"healthy", behaviour{}, time.Minute, 0, 5 * time.Millisecond, http.StatusOK},
		{"errors", behaviour{errorRate: 0.3}, time.Minute, 0.29, 5 * time.Millisecond, http.StatusInternalServerError},
		{"errors, lucky draw", behaviour{errorRate: 0.3}, time.Minute, 0.31, 5 * time.Millisecond, http.StatusOK},
		{"transient errors, still on", behaviour{errorRate: 0.1, errorFor: 7 * time.Minute}, 6 * time.Minute, 0.05, 5 * time.Millisecond, http.StatusInternalServerError},
		{"transient errors, over", behaviour{errorRate: 0.1, errorFor: 7 * time.Minute}, 8 * time.Minute, 0.05, 5 * time.Millisecond, http.StatusOK},
		{"noise only after a transient", behaviour{errorRate: 0.1, errorFor: 7 * time.Minute, noiseError: 0.06}, 8 * time.Minute, 0.05, 5 * time.Millisecond, http.StatusInternalServerError},
		// 1 - 0.9 × 0.9 = 0.19: both sources of errors combine.
		{"errors and noise combine", behaviour{errorRate: 0.1, noiseError: 0.1}, time.Minute, 0.185, 5 * time.Millisecond, http.StatusInternalServerError},
		{"slow", behaviour{latency: 300 * time.Millisecond}, time.Minute, 0, 305 * time.Millisecond, http.StatusOK},
		{"slow, recovered", behaviour{latency: 300 * time.Millisecond, latencyFor: 7 * time.Minute}, 8 * time.Minute, 0, 5 * time.Millisecond, http.StatusOK},
		{"tail", behaviour{tailShare: 0.01, tail: 200 * time.Millisecond}, time.Minute, 0.005, 205 * time.Millisecond, http.StatusOK},
		{"no tail", behaviour{tailShare: 0.01, tail: 200 * time.Millisecond}, time.Minute, 0.5, 5 * time.Millisecond, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, code := tc.b.outcome(tc.age, constant(tc.draw), zero)
			if delay != tc.delay || code != tc.code {
				t.Errorf("outcome %s %d, want %s %d", delay, code, tc.delay, tc.code)
			}
		})
	}
}

func TestDailyFactor(t *testing.T) {
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Duration
		want float64
	}{
		{14 * time.Hour, 1.5}, // peak
		{2 * time.Hour, 0.5},  // trough, twelve hours later
		{20 * time.Hour, 1},   // halfway down
		{24*time.Hour + 14*time.Hour, 1.5},
	} {
		if got := dailyFactor(day.Add(tc.at), 24*time.Hour, 0.5, 14*time.Hour); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("at %s: %.3f, want %.3f", tc.at, got, tc.want)
		}
	}
	if got := dailyFactor(day, 0, 0.5, 14*time.Hour); got != 1 {
		t.Errorf("no cycle: %v", got)
	}
}
