// Command app is the workload of the proactive mode's end-to-end test:
// an HTTP service instrumented the way the default PromQL templates
// expect (http_requests_total with a code label, and the
// http_request_duration_seconds histogram), which generates its own
// traffic so the test needs no load generator.
//
// ERROR_RATE (0 to 1) is the share of requests answered with a 500.
// LATENCY_MS adds that many milliseconds to every request.
// CRASH=true makes it exit at once, so its pods never become ready.
package main

import (
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// requestsPerSecond is the self-generated load: enough for the volume
// guard over the test's two-minute windows.
const requestsPerSecond = 20

func main() {
	if os.Getenv("CRASH") == "true" {
		log.Fatal("CRASH=true: exiting")
	}
	errorRate, _ := strconv.ParseFloat(os.Getenv("ERROR_RATE"), 64)
	extra, _ := strconv.Atoi(os.Getenv("LATENCY_MS"))

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "HTTP requests by status code.",
	}, []string{"code"})
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds", Help: "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(requests, duration)

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

	go load()
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// load calls /work on itself at a steady rate.
func load() {
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(time.Second / requestsPerSecond)
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
