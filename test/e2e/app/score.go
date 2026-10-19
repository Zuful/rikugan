package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// score compares a lab export's ground truth (actions.jsonl) with
// rikugan's journal, and writes report.md and notifications.csv next to
// them: what was detected, how fast, what was raised for nothing, and
// how often rikugan had too little data to say.
//
//	app score <export directory>
func score() {
	if len(os.Args) != 3 {
		log.Fatal("usage: app score <export directory>")
	}
	dir := os.Args[2]
	actions, err := readActions(filepath.Join(dir, "actions.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	records, err := readJournal(filepath.Join(dir, "journal"))
	if err != nil {
		log.Fatal(err)
	}
	s := scoreRun(actions, records)
	report, err := os.Create(filepath.Join(dir, "report.md")) // #nosec G304 G703 -- the operator's own export directory
	if err != nil {
		log.Fatal(err)
	}
	s.writeReport(report)
	if err := report.Close(); err != nil {
		log.Fatal(err)
	}
	notes, err := os.Create(filepath.Join(dir, "notifications.csv")) // #nosec G304 G703 -- the operator's own export directory
	if err != nil {
		log.Fatal(err)
	}
	if err := s.writeNotifications(notes); err != nil {
		log.Fatal(err)
	}
	if err := notes.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s and %s\n", filepath.Join(dir, "report.md"), filepath.Join(dir, "notifications.csv"))
}

// Criteria fixed before the run (deploy/lab/README.md).
const (
	minCalm         = 72 * time.Hour
	frankDelay      = 10 * time.Minute // errors, crashes, OOMs
	frankLatencyLag = 20 * time.Minute // latency is capped at investigate at +5
	// settle is how long after an injection ends its effects may still
	// show: checks and sweeps running on the window it spoiled.
	settle = 45 * time.Minute
)

type action struct {
	Time     time.Time `json:"time"`
	Action   string    `json:"action"`
	Service  string    `json:"service"`
	Scenario string    `json:"scenario"`
	Severity string    `json:"severity"`
	Signals  []string  `json:"signals"`
	Lasts    string    `json:"lasts"`
	Services []string  `json:"services"`
	Traffic  *float64  `json:"traffic"`
	Expected string    `json:"expected"`
}

type record struct {
	Time    time.Time       `json:"time"`
	Kind    string          `json:"kind"`
	Service string          `json:"service"`
	Data    json.RawMessage `json:"data"`
}

type signal struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func readActions(path string) ([]action, error) {
	f, err := os.Open(path) // #nosec G304 G703 -- the operator's own export directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []action
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var a action
		if json.Unmarshal(sc.Bytes(), &a) == nil && !a.Time.IsZero() {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, sc.Err()
}

// readJournal reads every journal file, rotated ones included, in time
// order.
func readJournal(dir string) ([]record, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no journal in %s", dir)
	}
	var out []record
	for _, path := range files {
		f, err := os.Open(path) // #nosec G304 G703 -- the operator's own export directory
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var r record
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				out = append(out, r)
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// injection is a regression the lab put in place: a bad revision, or a
// dependency down, on one service.
type injection struct {
	service  string
	scenario string
	severity string
	signals  []string
	// start and end bound the regression: end is the next deploy, the
	// dependency back, or the end of what it lasts on its own.
	start, end time.Time
	sequence   string // the rollout, for a deploy
	deploy     bool   // a bad revision, judged by rollout checks

	detected  bool
	reached   string // the highest status a truth signal got
	firstNote time.Time
}

func (in *injection) frank() bool {
	return in.severity == "degraded"
}

// allowedDelay is the criterion for the first notification.
func (in *injection) allowedDelay() time.Duration {
	for _, s := range in.signals {
		if s == "latency" {
			return frankLatencyLag
		}
	}
	return frankDelay
}

type notification struct {
	time      time.Time
	service   string // short name
	source    string // rollout or sweep
	severity  string
	title     string
	justified bool
}

type cell struct{ tp, fn, fp, tn, notEvaluated int }

type serviceData struct {
	checks, insufficient                int
	errorRateUnevaluated, latencyUneval int
	calm                                time.Duration
}

type scoring struct {
	from, to      time.Time
	injections    []*injection
	notifications []notification
	// cells by checkpoint, then by signal.
	cells    map[string]map[string]*cell
	services map[string]*serviceData
}

// short turns a journal service (lab-shop/web) into the lab's name.
func short(service string) string {
	if i := strings.LastIndex(service, "/"); i >= 0 {
		return service[i+1:]
	}
	return service
}

func flagged(status string) bool { return status == "investigate" || status == "degraded" }

func scoreRun(actions []action, records []record) *scoring {
	s := &scoring{cells: map[string]map[string]*cell{}, services: map[string]*serviceData{}}
	if len(actions) > 0 {
		s.from = actions[0].Time
	}
	if len(records) > 0 {
		s.to = records[len(records)-1].Time
	}
	if len(actions) > 0 && actions[len(actions)-1].Time.After(s.to) {
		s.to = actions[len(actions)-1].Time
	}

	// The truth: what was injected, when, until when.
	var current = map[string]*injection{} // the open deploy per service
	var dependency []*injection
	for _, a := range actions {
		switch a.Action {
		case "deploy":
			if prev := current[a.Service]; prev != nil && (prev.end.IsZero() || a.Time.Before(prev.end)) {
				prev.end = a.Time
			}
			delete(current, a.Service)
			if a.Severity == "" || a.Severity == "none" {
				continue
			}
			in := &injection{service: a.Service, scenario: a.Scenario, severity: a.Severity, signals: a.Signals, start: a.Time, deploy: true}
			if d, err := time.ParseDuration(a.Lasts); err == nil && d > 0 {
				in.end = a.Time.Add(d)
			}
			current[a.Service] = in
			s.injections = append(s.injections, in)
		case "dependency-stop":
			for _, svc := range a.Services {
				in := &injection{service: svc, scenario: "dependency down", severity: a.Severity, signals: a.Signals, start: a.Time}
				dependency = append(dependency, in)
				s.injections = append(s.injections, in)
			}
		case "dependency-start":
			for _, in := range dependency {
				if in.end.IsZero() {
					in.end = a.Time
				}
			}
		}
	}
	for _, in := range s.injections {
		if in.end.IsZero() {
			in.end = s.to
		}
	}

	// Rollouts: link each bad deploy to the rollout it started.
	type rollout struct {
		service, sequence string
		started           time.Time
	}
	var rollouts []rollout
	seen := map[string]bool{}
	for _, r := range records {
		if r.Kind != "rollout" {
			continue
		}
		var rr struct {
			Sequence  string    `json:"sequence"`
			StartedAt time.Time `json:"started_at"`
		}
		key := r.Service + "\x00"
		if json.Unmarshal(r.Data, &rr) != nil {
			continue
		}
		key += rr.Sequence
		if seen[key] {
			continue
		}
		seen[key] = true
		rollouts = append(rollouts, rollout{short(r.Service), rr.Sequence, rr.StartedAt})
	}
	deploys := map[string][]action{}
	for _, a := range actions {
		if a.Action == "deploy" {
			deploys[a.Service] = append(deploys[a.Service], a)
		}
	}
	// linkSeq maps a rollout to the injection its deploy carried;
	// healthySeq marks the rollouts of healthy deploys.
	linkSeq := map[string]*injection{} // by service + sequence
	healthySeq := map[string]bool{}
	for _, ro := range rollouts {
		var best *action
		for i, a := range deploys[ro.service] {
			if d := ro.started.Sub(a.Time); d > -2*time.Minute && d < 2*time.Minute {
				if best == nil || abs(d) < abs(ro.started.Sub(best.Time)) {
					best = &deploys[ro.service][i]
				}
			}
		}
		if best == nil {
			continue
		}
		key := ro.service + "\x00" + ro.sequence
		healthySeq[key] = best.Severity == "none" || best.Severity == ""
		for _, in := range s.injections {
			if in.service == ro.service && in.start.Equal(best.Time) {
				in.sequence = ro.sequence
				linkSeq[key] = in
			}
		}
	}

	// activeAt is the injection on svc at t, settle included.
	activeAt := func(svc string, t time.Time) *injection {
		for _, in := range s.injections {
			if in.service == svc && !t.Before(in.start) && t.Before(in.end.Add(settle)) {
				return in
			}
		}
		return nil
	}

	for _, r := range records {
		svc := short(r.Service)
		switch r.Kind {
		case "check":
			var c struct {
				Sequence   string   `json:"sequence"`
				Checkpoint string   `json:"checkpoint"`
				Verdict    string   `json:"verdict"`
				Outcome    string   `json:"outcome"`
				Partial    bool     `json:"partial"`
				Signals    []signal `json:"signals"`
			}
			if json.Unmarshal(r.Data, &c) != nil || c.Partial || c.Outcome != "evaluated" {
				continue
			}
			key := svc + "\x00" + c.Sequence
			in := linkSeq[key]
			if in == nil && !healthySeq[key] {
				continue // a rollout the lab did not start, or not linked
			}
			if in == nil {
				// A healthy rollout checked while its dependency is down.
				if d := activeAt(svc, r.Time); d != nil && d.sequence == "" {
					in = d
				}
			}
			sd := s.service(svc)
			sd.checks++
			if c.Verdict == "insufficient_data" {
				sd.insufficient++
			}
			for _, sig := range c.Signals {
				if sig.Status == "not_evaluated" {
					switch sig.Name {
					case "error_rate":
						sd.errorRateUnevaluated++
					case "latency":
						sd.latencyUneval++
					}
				}
			}
			s.tally(c.Checkpoint, c.Signals, in)
			if in != nil {
				in.observe(c.Signals)
			}
		case "sweep":
			var sw struct {
				Signals  []signal `json:"signals"`
				Notified *struct {
					Severity string `json:"severity"`
					Title    string `json:"title"`
				} `json:"notified"`
			}
			if json.Unmarshal(r.Data, &sw) != nil {
				continue
			}
			if in := activeAt(svc, r.Time); in != nil {
				in.observe(sw.Signals)
			}
			if sw.Notified != nil {
				s.notify(r.Time, svc, "sweep", sw.Notified.Severity, sw.Notified.Title, activeAt(svc, r.Time))
			}
		case "notification":
			var n struct {
				Sequence string `json:"sequence"`
				Severity string `json:"severity"`
				Title    string `json:"title"`
			}
			if json.Unmarshal(r.Data, &n) != nil {
				continue
			}
			in := linkSeq[svc+"\x00"+n.Sequence]
			if in == nil {
				in = activeAt(svc, r.Time)
			}
			s.notify(r.Time, svc, "rollout", n.Severity, n.Title, in)
		}
	}

	// Calm time per service: the run, minus its injections.
	names := map[string]bool{}
	for _, a := range actions {
		if a.Service != "" && a.Action == "deploy" {
			names[a.Service] = true
		}
		for _, svc := range a.Services {
			names[svc] = true
		}
	}
	for name := range names {
		busy := time.Duration(0)
		for _, in := range s.injections {
			if in.service == name {
				busy += in.end.Add(settle).Sub(in.start)
			}
		}
		s.service(name).calm = max(s.to.Sub(s.from)-busy, 0)
	}
	return s
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (s *scoring) service(name string) *serviceData {
	if s.services[name] == nil {
		s.services[name] = &serviceData{}
	}
	return s.services[name]
}

// observe records what a check or sweep said of the truth's signals.
func (in *injection) observe(signals []signal) {
	for _, sig := range signals {
		if !flagged(sig.Status) || !in.hasSignal(sig.Name) {
			continue
		}
		in.detected = true
		if in.reached != "degraded" {
			in.reached = sig.Status
		}
	}
}

func (in *injection) hasSignal(name string) bool {
	for _, s := range in.signals {
		if s == name {
			return true
		}
	}
	return false
}

// tally counts one check's signals against the truth, by checkpoint.
func (s *scoring) tally(checkpoint string, signals []signal, in *injection) {
	if s.cells[checkpoint] == nil {
		s.cells[checkpoint] = map[string]*cell{}
	}
	for _, sig := range signals {
		c := s.cells[checkpoint][sig.Name]
		if c == nil {
			c = &cell{}
			s.cells[checkpoint][sig.Name] = c
		}
		injected := in != nil && in.hasSignal(sig.Name)
		switch {
		case sig.Status == "not_evaluated":
			c.notEvaluated++
		case injected && flagged(sig.Status):
			c.tp++
		case injected:
			c.fn++
		case flagged(sig.Status):
			c.fp++
		default:
			c.tn++
		}
	}
}

func (s *scoring) notify(at time.Time, svc, source, severity, title string, in *injection) {
	n := notification{time: at, service: svc, source: source, severity: severity, title: title, justified: in != nil}
	if in != nil && severity != "info" && (in.firstNote.IsZero() || at.Before(in.firstNote)) {
		in.firstNote = at
	}
	s.notifications = append(s.notifications, n)
}

func (s *scoring) writeReport(w io.Writer) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("# Lab score\n\n")
	p("Run: %s → %s (%s).\n\n", s.from.UTC().Format(time.RFC3339), s.to.UTC().Format(time.RFC3339), s.to.Sub(s.from).Round(time.Minute))

	// Criteria.
	var calm time.Duration
	unjustifiedCritical, unjustifiedWarning := 0, 0
	for _, n := range s.notifications {
		if n.justified || n.severity == "info" {
			continue
		}
		if n.severity == "critical" {
			unjustifiedCritical++
		} else {
			unjustifiedWarning++
		}
	}
	for _, sd := range s.services {
		calm = max(calm, sd.calm)
	}
	frank, frankDetected, frankLate := 0, 0, 0
	for _, in := range s.injections {
		if !in.frank() {
			continue
		}
		frank++
		if in.detected {
			frankDetected++
		}
		// Only rollouts have a delay to meet: what sweeps find waits
		// for the next sweep.
		if in.deploy && (in.firstNote.IsZero() || in.firstNote.Sub(in.start) > in.allowedDelay()) {
			frankLate++
		}
	}
	p("## Criteria\n\n")
	verdict := func(ok bool) string {
		if ok {
			return "PASS"
		}
		return "FAIL"
	}
	calmNote := ""
	if calm < minCalm {
		calmNote = fmt.Sprintf(" — calm time %s, under the %s needed to conclude", calm.Round(time.Hour), minCalm)
	}
	p("- **%s** no critical without a regression behind it: %d (and %d warnings) over %s of calm%s.\n",
		verdict(unjustifiedCritical == 0), unjustifiedCritical, unjustifiedWarning, calm.Round(time.Hour), calmNote)
	p("- **%s** every frank regression detected: %d of %d.\n", verdict(frankDetected == frank), frankDetected, frank)
	p("- **%s** every frank regression deployed notified in time (%s, %s for latency): %d late or never.\n",
		verdict(frankLate == 0), frankDelay, frankLatencyLag, frankLate)
	p("- Insufficient data, by service (no threshold: the input of the fallback A vs B decision):\n\n")
	p("| Service | Checks | Insufficient data | Error rate not evaluated | Latency not evaluated | Calm time |\n|---|---|---|---|---|---|\n")
	for _, name := range sortedKeys(s.services) {
		sd := s.services[name]
		p("| %s | %d | %s | %s | %s | %s |\n", name, sd.checks, pct(sd.insufficient, sd.checks),
			pct(sd.errorRateUnevaluated, sd.checks), pct(sd.latencyUneval, sd.checks), sd.calm.Round(time.Hour))
	}

	p("\n## Injections\n\n| Start | Service | Scenario | Truth | Detected | Reached | First notification |\n|---|---|---|---|---|---|---|\n")
	for _, in := range s.injections {
		first := "none"
		if !in.firstNote.IsZero() {
			first = "+" + in.firstNote.Sub(in.start).Round(time.Second).String()
		}
		detected := "no"
		if in.detected {
			detected = "yes"
		}
		p("| %s | %s | %s | %s %s | %s | %s | %s |\n", in.start.UTC().Format("01-02 15:04"), in.service, in.scenario,
			in.severity, strings.Join(in.signals, ","), detected, orDash(in.reached), first)
	}

	p("\n## Signals by checkpoint (rollout checks)\n\nTP: injected and flagged; FN: injected, not flagged; FP: flagged without injection; TN; N/E: not evaluated.\n\n")
	p("| Checkpoint | Signal | TP | FN | FP | TN | N/E |\n|---|---|---|---|---|---|---|\n")
	for _, cp := range sortedKeys(s.cells) {
		for _, name := range sortedKeys(s.cells[cp]) {
			c := s.cells[cp][name]
			p("| %s | %s | %d | %d | %d | %d | %d |\n", cp, name, c.tp, c.fn, c.fp, c.tn, c.notEvaluated)
		}
	}

	p("\n## Notifications without a regression behind them\n\n")
	listed := false
	for _, n := range s.notifications {
		if n.justified || n.severity == "info" {
			continue
		}
		listed = true
		p("- %s %s [%s] %s (%s)\n", n.time.UTC().Format("01-02 15:04"), n.service, n.severity, n.title, n.source)
	}
	if !listed {
		p("None.\n")
	}
	p("\nEach notification is in notifications.csv, with an empty `actionable_at_4am` column to fill in: reading it at 4 a.m., would you know what to do?\n")
}

func (s *scoring) writeNotifications(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "service", "source", "severity", "title", "justified", "actionable_at_4am"})
	for _, n := range s.notifications {
		_ = cw.Write([]string{n.time.UTC().Format(time.RFC3339), n.service, n.source, n.severity, n.title, fmt.Sprint(n.justified), ""})
	}
	cw.Flush()
	return cw.Error()
}

func pct(n, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f %%", 100*float64(n)/float64(total))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
