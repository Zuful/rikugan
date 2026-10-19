package watch

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Zuful/rikugan/internal/watch/journal"
)

// Verdict is one check as the verdicts API returns it.
type Verdict struct {
	Time       time.Time `json:"time"`
	Service    string    `json:"service"`
	Source     string    `json:"source"`
	Revision   string    `json:"revision"`
	Checkpoint string    `json:"checkpoint"`
	Late       bool      `json:"late,omitempty"`
	// Partial is a check made with the metrics backend failing: only
	// the instance signals were read.
	Partial bool     `json:"partial,omitempty"`
	Outcome string   `json:"outcome"`
	Verdict string   `json:"verdict,omitempty"`
	Signals []Signal `json:"signals,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// VerdictsResponse is the body of GET /v1/verdicts.
type VerdictsResponse struct {
	Verdicts []Verdict `json:"verdicts"`
}

const (
	defaultVerdictLimit = 100
	maxVerdictLimit     = 1000
)

// CheckListen enforces the start-up rule of the verdicts API (design
// §4): on a loopback address the token is optional; on any other
// address, an empty host (":8080") included, a token is required, or
// the API would be readable by anyone on the network.
func CheckListen(addr, token string) error {
	if token != "" || isLoopback(addr) {
		return nil
	}
	return fmt.Errorf("refusing to listen on %s without RIKUGAN_SERVE_TOKEN: the verdicts API would be readable by anyone on the network. Set a token, or listen on 127.0.0.1", addr)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewAPI returns the read-only HTTP API over the journal:
//
//	GET /v1/verdicts?service=<namespace/name or name>&since=<RFC 3339 or duration>&limit=<n>
//	GET /healthz
//
// Verdicts come newest first. With a token, /v1/verdicts requires
// "Authorization: Bearer <token>", compared in constant time; /healthz
// never does and carries no data.
func NewAPI(j *journal.Journal, token string, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/verdicts", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && !authorized(r, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="rikugan"`)
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		q, problem := parseVerdictQuery(r, now())
		if problem != "" {
			http.Error(w, problem, http.StatusBadRequest)
			return
		}
		verdicts, err := readVerdicts(j, q)
		if err != nil {
			http.Error(w, "cannot read the journal", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VerdictsResponse{Verdicts: verdicts})
	})
	return mux
}

func authorized(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

type verdictQuery struct {
	service string // namespace/name, or a bare name matching any namespace
	since   time.Time
	limit   int
}

// parseVerdictQuery reads the query parameters. A bad one is reported
// with a fixed message naming the parameter, never with an error's text.
func parseVerdictQuery(r *http.Request, now time.Time) (verdictQuery, string) {
	q := verdictQuery{service: r.URL.Query().Get("service"), since: now.Add(-24 * time.Hour), limit: defaultVerdictLimit}
	if raw := r.URL.Query().Get("since"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			q.since = now.Add(-d)
		} else if t, err := time.Parse(time.RFC3339, raw); err == nil {
			q.since = t
		} else {
			return q, "since: want an RFC 3339 time or a duration such as 6h"
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return q, "limit: want a positive integer"
		}
		q.limit = min(n, maxVerdictLimit)
	}
	return q, ""
}

func (q verdictQuery) matches(service string) bool {
	switch {
	case q.service == "":
		return true
	case strings.Contains(q.service, "/"):
		return service == q.service
	default:
		_, name, _ := strings.Cut(service, "/")
		return name == q.service
	}
}

// readVerdicts scans the journal for check records. The journal is
// append-only and Replay reads it through its own file handles, so this
// runs alongside the daemon writing to it.
func readVerdicts(j *journal.Journal, q verdictQuery) ([]Verdict, error) {
	var out []Verdict
	_, err := j.Replay(func(rec journal.Record) error {
		if (rec.Kind != KindCheck && rec.Kind != KindSweep) || rec.Time.Before(q.since) || !q.matches(rec.Service) {
			return nil
		}
		if rec.Kind == KindSweep {
			var sw sweepRecord
			if json.Unmarshal(rec.Data, &sw) == nil {
				out = append(out, Verdict{
					Time: rec.Time, Service: rec.Service, Source: "sweep", Revision: sw.Revision,
					Outcome: sw.Outcome, Verdict: sw.Verdict, Signals: sw.Signals, Notes: sw.Notes,
				})
			}
			return nil
		}
		var c checkRecord
		if json.Unmarshal(rec.Data, &c) != nil {
			return nil
		}
		out = append(out, Verdict{
			Time: rec.Time, Service: rec.Service, Source: "rollout", Revision: c.Sequence,
			Checkpoint: c.Checkpoint, Late: c.Late, Partial: c.Partial, Outcome: c.Outcome, Verdict: c.Verdict,
			Signals: c.Signals, Notes: c.Notes,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Newest first; among equal times, the last recorded first.
	slices.Reverse(out)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Time.After(out[b].Time) })
	if len(out) > q.limit {
		out = out[:q.limit]
	}
	return out, nil
}
