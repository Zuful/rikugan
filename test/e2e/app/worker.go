package main

import (
	"crypto/sha256"
	"log"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// worker is a service without HTTP traffic, a queue consumer say: it
// processes JOBS_PER_MIN jobs a minute (default 30) and exposes only
// its process metrics and a jobs counter. Rikugan has no request rate
// for it, so only restarts, availability and resources can be judged.
// CRASH, OOM and LEAK_MB_PER_MIN apply as for the HTTP service.
func worker() {
	faults()
	perMin := envFloat("JOBS_PER_MIN", 30)
	jobs := prometheus.NewCounter(prometheus.CounterOpts{Name: "worker_jobs_total", Help: "Jobs processed."})
	reg := prometheus.NewRegistry()
	reg.MustRegister(jobs, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if perMin > 0 {
		go func() {
			for range time.Tick(time.Duration(float64(time.Minute) / perMin)) {
				busy(time.Duration(20+rand.IntN(30)) * time.Millisecond) // #nosec G404 -- test workload
				jobs.Inc()
			}
		}()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// busy burns CPU for d, as a job would.
func busy(d time.Duration) {
	sum := sha256.Sum256(nil)
	for end := time.Now().Add(d); time.Now().Before(end); {
		sum = sha256.Sum256(sum[:])
	}
}
