// Command app is the workload of the proactive mode's end-to-end test
// and of the local lab (deploy/lab): an HTTP service instrumented the
// way the default PromQL templates expect (http_requests_total with a
// code label, and the http_request_duration_seconds histogram), plus
// the process collector for memory and CPU.
//
// What a revision does wrong (the lab's scenarios):
//
//   - ERROR_RATE (0 to 1): the share of requests answered with a 500;
//   - LATENCY_MS: milliseconds added to every request;
//   - ERROR_FOR, LATENCY_FOR: how long after the start those last
//     (default: for good), for a regression that recovers;
//   - CRASH=true: exit at once, so the instance never becomes ready;
//   - OOM=true: allocate until the container's limit kills it;
//   - LEAK_MB_PER_MIN: retain that many MiB more every minute;
//   - ERROR_LOG: a line logged with the 500s, at most every 10 seconds,
//     for what the logs say (the lab's injection canary writes one).
//
// Background noise, what a healthy service does anyway:
//
//   - NOISE_ERROR_RATE: sporadic 5xx;
//   - TAIL_SHARE, TAIL_MS: a share of requests that take TAIL_MS more;
//   - PAUSE_EVERY, PAUSE_MS: a stop-the-world pause, as a GC would.
//
// DEPENDENCY_URL, when set, is called by every request; when it fails,
// the request is answered with a 503, as a service whose database is
// down. LOAD_RPS is the traffic it generates on itself (default 20, so
// the end-to-end test needs no load generator); 0 leaves the load to
// `app loadgen`.
//
// `app healthcheck` probes /healthz, for images without a shell; `app
// loadgen` sends traffic to other instances (loadgen.go); `app worker`
// is a service without HTTP traffic (worker.go); `app score` compares a
// lab export with its ground truth (score.go); `app history` writes a
// synthetic past for Prometheus (history.go).
package main

import (
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
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
		case "loadgen":
			loadgen()
		case "worker":
			worker()
		case "score":
			score()
		case "history":
			history()
		default:
			log.Fatal("unknown command: want healthcheck, loadgen, worker, score or history")
		}
		return
	}
	faults()
	b := behaviourFromEnv(time.Now())
	rps := envFloat("LOAD_RPS", defaultLoadRPS)

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "HTTP requests by status code.",
	}, []string{"code"})
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds", Help: "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(requests, duration, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	var pause sync.RWMutex
	if b.pauseEvery > 0 {
		go pauses(&pause, b.pauseEvery, b.pauseFor)
	}
	dep := &http.Client{Timeout: time.Second}
	errorLog := throttled(os.Getenv("ERROR_LOG"), 10*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/work", func(w http.ResponseWriter, _ *http.Request) {
		start := time.Now()
		// A pause, as a stop-the-world GC, holds every request: it waits
		// for those in flight and keeps new ones until it ends.
		pause.RLock()
		defer pause.RUnlock()
		delay, code := b.outcome(start.Sub(b.started), rand.Float64, rand.IntN) // #nosec G404 -- test workload
		if code == http.StatusOK && b.dependency != "" && !reachable(dep, b.dependency) {
			code = http.StatusServiceUnavailable
		}
		if code == http.StatusInternalServerError {
			errorLog(start)
		}
		time.Sleep(delay)
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

// faults applies the faults any kind of instance can have: a crash at
// start, an OOM, a leak.
func faults() {
	if os.Getenv("CRASH") == "true" {
		log.Fatal("CRASH=true: exiting")
	}
	if os.Getenv("OOM") == "true" {
		go allocate(64, 100*time.Millisecond)
	}
	if mb := envInt("LEAK_MB_PER_MIN"); mb > 0 {
		go allocate(mb, time.Minute)
	}
}

// behaviour is what a request does: the revision's faults and the
// background noise.
type behaviour struct {
	started    time.Time
	errorRate  float64
	errorFor   time.Duration // 0: for good
	latency    time.Duration
	latencyFor time.Duration
	noiseError float64
	tailShare  float64
	tail       time.Duration
	pauseEvery time.Duration
	pauseFor   time.Duration
	dependency string
}

func behaviourFromEnv(now time.Time) behaviour {
	return behaviour{
		started:    now,
		errorRate:  envFloat("ERROR_RATE", 0),
		errorFor:   envDuration("ERROR_FOR", 0),
		latency:    time.Duration(envInt("LATENCY_MS")) * time.Millisecond,
		latencyFor: envDuration("LATENCY_FOR", 0),
		noiseError: envFloat("NOISE_ERROR_RATE", 0),
		tailShare:  envFloat("TAIL_SHARE", 0),
		tail:       time.Duration(envInt("TAIL_MS")) * time.Millisecond,
		pauseEvery: envDuration("PAUSE_EVERY", 0),
		pauseFor:   time.Duration(envInt("PAUSE_MS")) * time.Millisecond,
		dependency: os.Getenv("DEPENDENCY_URL"),
	}
}

// outcome draws a request's delay and status code, age after the
// start. float and intn are the random source, injected for the tests.
func (b behaviour) outcome(age time.Duration, float func() float64, intn func(int) int) (time.Duration, int) {
	delay := time.Duration(5+intn(10)) * time.Millisecond
	if b.latencyFor == 0 || age < b.latencyFor {
		delay += b.latency
	}
	if b.tailShare > 0 && float() < b.tailShare {
		delay += b.tail
	}
	errorRate := b.noiseError
	if b.errorFor == 0 || age < b.errorFor {
		errorRate = 1 - (1-b.errorRate)*(1-b.noiseError)
	}
	if errorRate > 0 && float() < errorRate {
		return delay, http.StatusInternalServerError
	}
	return delay, http.StatusOK
}

// throttled returns a function logging line at most once every
// interval; an empty line logs nothing.
func throttled(line string, interval time.Duration) func(now time.Time) {
	var mu sync.Mutex
	var last time.Time
	return func(now time.Time) {
		if line == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !last.IsZero() && now.Sub(last) < interval {
			return
		}
		last = now
		log.Print(line)
	}
}

// pauses stops every request for d, every interval.
func pauses(pause *sync.RWMutex, every, d time.Duration) {
	for range time.Tick(every) {
		pause.Lock()
		time.Sleep(d)
		pause.Unlock()
	}
}

func reachable(c *http.Client, url string) bool {
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
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

func envInt(name string) int {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Fatalf("%s: want a non-negative integer", name)
	}
	return n
}
