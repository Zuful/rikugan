package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// history writes the lab's past as OpenMetrics, for promtool to load
// into Prometheus (tsdb create-blocks-from openmetrics): the sweep
// compares each hour with the same hour a day and a week earlier, and
// waiting a week for that baseline is not testing. The traffic follows
// the loadgen's daily cycle and the requests the app's own draw, noise
// included, so the past looks like what the lab does now.
//
//	app history -days 8 -end 2026-10-20T08:00:00Z -out /out/history.om
func history() {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	days := fs.Int("days", 8, "days of history")
	endRaw := fs.String("end", "", "end of the history, RFC 3339 or Unix seconds (default: now)")
	out := fs.String("out", "", "output file (default: standard output)")
	step := fs.Duration("step", 15*time.Second, "sample interval, the lab's scrape interval")
	_ = fs.Parse(os.Args[2:])
	end := time.Now()
	if *endRaw != "" {
		if secs, err := strconv.ParseInt(*endRaw, 10, 64); err == nil {
			end = time.Unix(secs, 0)
		} else if end, err = time.Parse(time.RFC3339, *endRaw); err != nil {
			log.Fatal("-end: want an RFC 3339 time or Unix seconds")
		}
	}
	w := io.Writer(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out) // #nosec G304 -- the operator's path
		if err != nil {
			log.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	cfg := historyConfigFromEnv()
	if err := writeHistory(bw, cfg, end.Add(-time.Duration(*days)*24*time.Hour), end, *step, rand.New(rand.NewPCG(1, 2))); err != nil { // #nosec G404 -- test data
		log.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		log.Fatal(err)
	}
}

// historyConfig is the lab's shape: the same settings as the loadgen
// and the services' noise.
type historyConfig struct {
	namespace string
	rates     map[string]float64 // mean requests per second, by service
	cycle     time.Duration
	amplitude float64
	peakAt    time.Duration
	noise     behaviour
	// callers lists the services whose requests each call dependency.
	callers map[string][]string
	workers []string
	jobsMin float64
}

func historyConfigFromEnv() historyConfig {
	targets, err := parseTargets(os.Getenv("TARGETS"))
	if err != nil {
		log.Fatalf("TARGETS: %v", err)
	}
	rates := map[string]float64{}
	for _, t := range targets {
		host := strings.TrimPrefix(t.url, "http://")
		rates[strings.Split(host, ":")[0]] = t.rps
	}
	return historyConfig{
		namespace: envOr("NAMESPACE", "lab-shop"),
		rates:     rates,
		cycle:     envDuration("CYCLE", 24*time.Hour),
		amplitude: envFloat("AMPLITUDE", 0.5),
		peakAt:    envDuration("PEAK_AT", 14*time.Hour),
		noise: behaviour{
			noiseError: envFloat("NOISE_ERROR_RATE", 0),
			tailShare:  envFloat("TAIL_SHARE", 0),
			tail:       time.Duration(envInt("TAIL_MS")) * time.Millisecond,
		},
		callers: map[string][]string{"db": {"web", "api"}},
		workers: []string{"worker"},
		jobsMin: envFloat("JOBS_PER_MIN", 30),
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// buckets are the app's histogram buckets (prometheus.DefBuckets).
var buckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// httpSeries is one service's counters over time.
type httpSeries struct {
	ok, errors []float64
	buckets    [][]float64 // by bucket, cumulative per le
	sum, count []float64
}

func writeHistory(w io.Writer, cfg historyConfig, from, to time.Time, step time.Duration, rng *rand.Rand) error {
	n := int(to.Sub(from) / step)
	if n <= 0 {
		return fmt.Errorf("nothing to write between %s and %s", from, to)
	}
	times := make([]time.Time, n)
	for i := range times {
		times[i] = from.Add(time.Duration(i+1) * step)
	}

	// Requests per step, by service: the callers first, then the
	// dependencies they call.
	served := map[string][]float64{}
	for svc, rps := range cfg.rates {
		counts := make([]float64, n)
		for i, t := range times {
			mean := rps * dailyFactor(t, cfg.cycle, cfg.amplitude, cfg.peakAt) * step.Seconds()
			counts[i] = poisson(rng, mean)
		}
		served[svc] = counts
	}
	for dep, callers := range cfg.callers {
		counts := make([]float64, n)
		for _, c := range callers {
			for i, v := range served[c] {
				counts[i] += v
			}
		}
		served[dep] = counts
	}

	series := map[string]*httpSeries{}
	for svc, counts := range served {
		s := &httpSeries{ok: make([]float64, n), errors: make([]float64, n), sum: make([]float64, n), count: make([]float64, n)}
		s.buckets = make([][]float64, len(buckets)+1)
		for b := range s.buckets {
			s.buckets[b] = make([]float64, n)
		}
		noise := cfg.noise
		if _, dep := cfg.callers[svc]; dep {
			noise = behaviour{} // the dependency carries no noise of its own
		}
		var ok, errs, sum float64
		perBucket := make([]float64, len(buckets)+1)
		for i, c := range counts {
			for range int(c) {
				delay, code := noise.outcome(time.Hour, rng.Float64, rng.IntN)
				if code == http.StatusOK {
					ok++
				} else {
					errs++
				}
				d := delay.Seconds()
				sum += d
				perBucket[sort.SearchFloat64s(buckets, d)]++
			}
			s.ok[i], s.errors[i], s.sum[i] = ok, errs, sum
			s.count[i] = ok + errs
			cum := 0.0
			for b := range perBucket {
				cum += perBucket[b]
				s.buckets[b][i] = cum
			}
		}
		series[svc] = s
	}

	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	labels := func(svc string) string {
		return fmt.Sprintf(`namespace="%s",service="%s",job="shop",instance="history"`, cfg.namespace, svc)
	}
	ts := func(i int) string { return fmt.Sprintf("%d", times[i].Unix()) }
	names := sortedKeys(series)

	p("# TYPE http_requests counter\n")
	for _, svc := range names {
		for _, code := range []struct {
			code string
			v    []float64
		}{{"200", series[svc].ok}, {"500", series[svc].errors}} {
			for i, v := range code.v {
				p("http_requests_total{%s,code=\"%s\"} %g %s\n", labels(svc), code.code, v, ts(i))
			}
		}
	}
	p("# TYPE http_request_duration_seconds histogram\n")
	for _, svc := range names {
		s := series[svc]
		for b := range s.buckets {
			le := "+Inf"
			if b < len(buckets) {
				le = fmt.Sprintf("%g", buckets[b])
			}
			for i, v := range s.buckets[b] {
				p("http_request_duration_seconds_bucket{%s,le=\"%s\"} %g %s\n", labels(svc), le, v, ts(i))
			}
		}
		for i, v := range s.sum {
			p("http_request_duration_seconds_sum{%s} %g %s\n", labels(svc), v, ts(i))
		}
		for i, v := range s.count {
			p("http_request_duration_seconds_count{%s} %g %s\n", labels(svc), v, ts(i))
		}
	}

	// Process metrics for every service, the worker's jobs.
	all := append(append([]string{}, names...), cfg.workers...)
	sort.Strings(all)
	p("# TYPE process_resident_memory_bytes gauge\n")
	for _, svc := range all {
		for i := range times {
			p("process_resident_memory_bytes{%s} %g %s\n", labels(svc), math.Round(22e6+rng.Float64()*2e6), ts(i))
		}
	}
	p("# TYPE process_cpu_seconds counter\n")
	for _, svc := range all {
		cpu := 0.0
		for i := range times {
			load := 0.002
			if s := served[svc]; s != nil {
				load += s[i] * 0.0004 // a fraction of a millisecond per request
			}
			cpu += load * step.Seconds()
			p("process_cpu_seconds_total{%s} %g %s\n", labels(svc), cpu, ts(i))
		}
	}
	p("# TYPE worker_jobs counter\n")
	for _, svc := range cfg.workers {
		jobs := 0.0
		for i := range times {
			jobs += poisson(rng, cfg.jobsMin*step.Minutes())
			p("worker_jobs_total{%s} %g %s\n", labels(svc), jobs, ts(i))
		}
	}
	p("# EOF\n")
	return nil
}

// poisson draws a Poisson count of the given mean; a normal
// approximation above 30.
func poisson(rng *rand.Rand, mean float64) float64 {
	if mean <= 0 {
		return 0
	}
	if mean > 30 {
		return math.Max(0, math.Round(mean+math.Sqrt(mean)*rng.NormFloat64()))
	}
	l, k, p := math.Exp(-mean), 0.0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}
