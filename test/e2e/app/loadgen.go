package main

import (
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// loadgen sends traffic to the lab's services, shaped like a real day
// rather than the steady self-load of the end-to-end test:
//
//   - TARGETS: comma-separated url=rps, e.g.
//     http://web:8080/work=20,http://api:8080/work=3 (rps is the mean);
//   - CYCLE: the period of a sine on the rate (default 24h, 0 for flat),
//     with AMPLITUDE its relative size (default 0.5: from 50 % to 150 %);
//   - BURST_EVERY: the mean time between 5-minute bursts at three times
//     the rate (default 0, none);
//   - NOISE: the relative size of minute-to-minute jitter (default 0.1).
//
// Keep-alives are off so each request resolves the name again and
// spreads over the replicas Docker's DNS returns.
func loadgen() {
	targets, err := parseTargets(os.Getenv("TARGETS"))
	if err != nil {
		log.Fatalf("TARGETS: %v", err)
	}
	cycle := envDuration("CYCLE", 24*time.Hour)
	amplitude := envFloat("AMPLITUDE", 0.5)
	burstEvery := envDuration("BURST_EVERY", 0)
	noise := envFloat("NOISE", 0.1)

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	start := time.Now()
	var burstUntil atomic.Int64 // unix nanoseconds
	for _, t := range targets {
		go func() {
			jitter, jitterAt := 1.0, time.Time{}
			for {
				now := time.Now()
				if now.Sub(jitterAt) >= time.Minute {
					jitter, jitterAt = 1+noise*(2*rand.Float64()-1), now // #nosec G404 -- test workload
				}
				factor := jitter
				if cycle > 0 {
					factor *= 1 + amplitude*math.Sin(2*math.Pi*float64(now.Sub(start))/float64(cycle))
				}
				if now.UnixNano() < burstUntil.Load() {
					factor *= 3
				}
				rps := math.Max(t.rps*factor, 0.01)
				time.Sleep(time.Duration(rand.ExpFloat64() / rps * float64(time.Second))) // #nosec G404 -- test workload
				go func() {
					resp, err := client.Get(t.url)
					if err == nil {
						_ = resp.Body.Close()
					}
				}()
			}
		}()
	}
	log.Printf("loadgen: %d targets, cycle %s (±%.0f %%), bursts every %s on average", len(targets), cycle, amplitude*100, burstEvery)
	for tick := time.NewTicker(time.Minute); ; <-tick.C {
		if burstEvery > 0 && rand.Float64() < float64(time.Minute)/float64(burstEvery) { // #nosec G404 -- test workload
			burstUntil.Store(time.Now().Add(5 * time.Minute).UnixNano())
			log.Printf("burst: three times the rate for 5 minutes")
		}
	}
}

type target struct {
	url string
	rps float64
}

func parseTargets(raw string) ([]target, error) {
	var out []target
	for _, item := range strings.Split(raw, ",") {
		url, rate, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			return nil, fmt.Errorf("want url=rps, got %q", item)
		}
		rps, err := strconv.ParseFloat(rate, 64)
		if err != nil || rps <= 0 {
			return nil, fmt.Errorf("want a positive rate in %q", item)
		}
		out = append(out, target{url: url, rps: rps})
	}
	return out, nil
}

func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Fatalf("%s: want a duration such as 24h, got %q", name, raw)
	}
	return d
}

func envFloat(name string, def float64) float64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		log.Fatalf("%s: want a non-negative number, got %q", name, raw)
	}
	return f
}
