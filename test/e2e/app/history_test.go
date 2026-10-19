package main

import (
	"bufio"
	"bytes"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWriteHistory(t *testing.T) {
	cfg := historyConfig{
		namespace: "lab-shop",
		rates:     map[string]float64{"web": 20, "api": 3},
		cycle:     24 * time.Hour, amplitude: 0.5, peakAt: 14 * time.Hour,
		noise:   behaviour{noiseError: 0.01},
		callers: map[string][]string{"db": {"web", "api"}},
		workers: []string{"worker"},
		jobsMin: 30,
	}
	from := time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC) // around the peak: ×1.5
	var out bytes.Buffer
	if err := writeHistory(&out, cfg, from, from.Add(time.Hour), 15*time.Second, rand.New(rand.NewPCG(1, 2))); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.HasSuffix(text, "# EOF\n") {
		t.Error("no # EOF at the end")
	}

	// Each family's samples are contiguous, each series in time order,
	// counters never go down.
	last := map[string]float64{}
	lastTS := map[string]int64{}
	families, seen := []string{}, map[string]bool{}
	family := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "# TYPE ") {
			family = strings.Fields(line)[2]
			if seen[family] {
				t.Errorf("family %s declared twice", family)
			}
			seen[family] = true
			families = append(families, family)
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !strings.HasPrefix(f[0], family) {
			t.Fatalf("sample %q outside its family %s", line, family)
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		if ts <= lastTS[f[0]] {
			t.Fatalf("%s not in time order", f[0])
		}
		if !strings.HasPrefix(f[0], "process_resident") && v < last[f[0]] {
			t.Fatalf("counter %s went down", f[0])
		}
		last[f[0]], lastTS[f[0]] = v, ts
	}
	if got := strings.Join(families, " "); got != "http_requests http_request_duration_seconds process_resident_memory_bytes process_cpu_seconds worker_jobs" {
		t.Errorf("families %q", got)
	}

	sel := `namespace="lab-shop",service="web",job="shop",instance="history"`
	ok, errs := last["http_requests_total{"+sel+`,code="200"}`], last["http_requests_total{"+sel+`,code="500"}`]
	// About 20 × 1.5 × 3600 = 108,000 requests in the hour around the peak.
	if total := ok + errs; total < 100_000 || total > 112_000 {
		t.Errorf("web served %.0f requests in the peak hour, want about 108,000", total)
	}
	if share := errs / (ok + errs); share < 0.008 || share > 0.012 {
		t.Errorf("error share %.4f, want the 1 %% noise", share)
	}
	if count := last["http_request_duration_seconds_count{"+sel+"}"]; count != ok+errs {
		t.Errorf("histogram count %.0f, requests %.0f", count, ok+errs)
	}
	if inf := last["http_request_duration_seconds_bucket{"+sel+`,le="+Inf"}`]; inf != ok+errs {
		t.Errorf("+Inf bucket %.0f, requests %.0f", inf, ok+errs)
	}
	// Requests take 5 to 14 whole milliseconds: 6 in 10 at 10 ms or less.
	if le10ms := last["http_request_duration_seconds_bucket{"+sel+`,le="0.01"}`]; le10ms < 0.57*(ok+errs) || le10ms > 0.63*(ok+errs) {
		t.Errorf("%.0f requests at 10 ms or less out of %.0f, want 60 %%", le10ms, ok+errs)
	}
	dbSel := `namespace="lab-shop",service="db",job="shop",instance="history"`
	dbTotal := last["http_requests_total{"+dbSel+`,code="200"}`] + last["http_requests_total{"+dbSel+`,code="500"}`]
	apiSel := `namespace="lab-shop",service="api",job="shop",instance="history"`
	callers := ok + errs + last["http_requests_total{"+apiSel+`,code="200"}`] + last["http_requests_total{"+apiSel+`,code="500"}`]
	if dbTotal != callers {
		t.Errorf("db served %.0f, its callers sent %.0f", dbTotal, callers)
	}
	if last["http_requests_total{"+dbSel+`,code="500"}`] != 0 {
		t.Error("the dependency carries noise of its own")
	}
	if jobs := last[`worker_jobs_total{namespace="lab-shop",service="worker",job="shop",instance="history"}`]; jobs < 1600 || jobs > 2000 {
		t.Errorf("worker jobs %.0f, want about 1,800", jobs)
	}
}

func TestPoisson(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, mean := range []float64{0.5, 5, 300} {
		sum := 0.0
		for range 20000 {
			sum += poisson(rng, mean)
		}
		if got := sum / 20000; got < mean*0.97 || got > mean*1.03 {
			t.Errorf("mean %.3f, want %.1f", got, mean)
		}
	}
}
