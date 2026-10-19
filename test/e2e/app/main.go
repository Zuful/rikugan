// Command app is the workload of the proactive mode's end-to-end test
// and of the local lab (deploy/lab): an HTTP service instrumented the
// way the default PromQL templates expect (http_requests_total with a
// code label, and the http_request_duration_seconds histogram), plus
// the process collector for memory and CPU.
//
// ERROR_RATE (0 to 1) is the share of requests answered with a 500.
// LATENCY_MS adds that many milliseconds to every request.
// CRASH=true makes it exit at once, so its pods never become ready.
// OOM=true allocates memory until the container's limit kills it.
// LEAK_MB_PER_MIN retains that many MiB more every minute.
// LOAD_RPS is the traffic it generates on itself (default 20, so the
// test needs no load generator); 0 leaves the load to `app loadgen`.
//
// `app healthcheck` probes /healthz, for images without a shell, and
// `app loadgen` sends traffic to other instances (see loadgen.go).
package main

import (
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// defaultLoadRPS is the self-generated load: enough for the volume
// guard over the test's two-minute windows.
const defaultLoadRPS = 20

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			healthcheck()
			return
		case "loadgen":
			loadgen()
			return
		default:
			log.Fatal("unknown command: want healthcheck or loadgen")
		}
	}
	if os.Getenv("CRASH") == "true" {
		log.Fatal("CRASH=true: exiting")
	}
	if os.Getenv("OOM") == "true" {
		go allocate(64, 100*time.Millisecond)
	}
	if mb, _ := strconv.Atoi(os.Getenv("LEAK_MB_PER_MIN")); mb > 0 {
		go allocate(mb, time.Minute)
	}
	errorRate, _ := strconv.ParseFloat(os.Getenv("ERROR_RATE"), 64)
	extra, _ := strconv.Atoi(os.Getenv("LATENCY_MS"))
	rps := float64(defaultLoadRPS)
	if raw := os.Getenv("LOAD_RPS"); raw != "" {
		var err error
		if rps, err = strconv.ParseFloat(raw, 64); err != nil || rps < 0 {
			log.Fatal("LOAD_RPS: want a non-negative number of requests per second")
		}
	}

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "HTTP requests by status code.",
	}, []string{"code"})
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds", Help: "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(requests, duration, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	mux := http.NewServeMux()
	mux.HandleFunc("/work", func(w http.ResponseWriter, _ *http.Request) {
		start := time.Now()
		time.Sleep(time.Duration(5+extra+rand.IntN(10)) * time.Millisecond) // #nosec G404 -- test workload
		code := http.StatusOK
		if rand.Float64() < errorRate { // #nosec G404 -- test workload
			code = http.StatusInternalServerError
		}
		w.WriteHeader(code)
		requests.WithLabelValues(strconv.Itoa(code)).Inc()
		duration.Observe(time.Since(start).Seconds())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	if rps > 0 {
		go load(rps)
	}
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// load calls /work on itself at a steady rate.
func load(rps float64) {
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(time.Duration(float64(time.Second) / rps))
	defer tick.Stop()
	for range tick.C {
		go func() {
			resp, err := client.Get("http://127.0.0.1:8080/work")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
}

// retained keeps allocate's memory reachable.
var retained [][]byte

// allocate retains mb MiB more every interval, touching every page so
// the memory is resident, not just reserved.
func allocate(mb int, every time.Duration) {
	for {
		b := make([]byte, mb<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		retained = append(retained, b)
		time.Sleep(every)
	}
}

// healthcheck exits 0 when /healthz answers 200.
func healthcheck() {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:8080/healthz")
	if err != nil {
		log.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("healthz: HTTP %d", resp.StatusCode)
	}
}
