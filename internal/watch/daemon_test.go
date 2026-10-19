package watch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/watch/journal"
	"github.com/Zuful/rikugan/pkg/provider"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

var api = provider.Service{Name: "api", Namespace: "prod", Kind: "deployment"}

type fakeSource struct {
	provider.DeploymentSource
	mu      sync.Mutex
	current provider.Rollout
	// bySvc overrides current per namespace/name.
	bySvc  map[string]provider.Rollout
	events chan provider.Rollout
}

func (f *fakeSource) LastRollout(_ context.Context, svc provider.Service) (provider.Rollout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.bySvc[serviceKey(svc)]; ok {
		return r, nil
	}
	return f.current, nil
}

func (f *fakeSource) WatchRollouts(context.Context) (<-chan provider.Rollout, error) {
	return f.events, nil
}

type call struct {
	seq       string
	cp        time.Duration
	beforeEnd time.Time
}

type fakeChecker struct {
	mu    sync.Mutex
	calls []call
	fail  bool
	// verdict is the verdict reported (default ok); err, when set, is
	// returned along with the report, as the collector does with a
	// failing metrics backend.
	verdict health.Status
	err     error
}

func (f *fakeChecker) CheckRollout(_ context.Context, r provider.Rollout, opts health.CheckOptions) (health.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return health.Report{}, errors.New("prometheus unreachable")
	}
	f.calls = append(f.calls, call{r.Label(), opts.Checkpoint, opts.BeforeEnd})
	v := f.verdict
	if v == "" {
		v = health.StatusOK
	}
	return health.Report{Rollout: r, Outcome: health.OutcomeEvaluated, Verdict: v, Checkpoint: opts.Checkpoint}, f.err
}

func (f *fakeChecker) seen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, fmt.Sprintf("%s@%s", c.seq, c.cp))
	}
	return strings.Join(out, " ")
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type harness struct {
	t       *testing.T
	dir     string
	clock   *clock
	source  *fakeSource
	checker *fakeChecker
	checks  []Check
	d       *Daemon
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, dir: t.TempDir(), clock: &clock{now: t0}, source: &fakeSource{}, checker: &fakeChecker{}}
	h.restart()
	return h
}

// restart builds a fresh daemon on the same journal, as after a crash.
func (h *harness) restart() {
	if h.d != nil {
		_ = h.d.Journal.Close()
	}
	j, err := journal.Open(h.dir, journal.Options{Now: h.clock.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = j.Close() })
	h.d = &Daemon{
		Source: h.source, Checker: h.checker, Journal: j, Now: h.clock.Now,
		OnCheck: func(_ context.Context, c Check) { h.checks = append(h.checks, c) },
	}
	if err := h.d.Replay(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) at(d time.Duration) *harness {
	h.clock.now = t0.Add(d)
	return h
}

func (h *harness) handle(r provider.Rollout) {
	h.t.Helper()
	if err := h.d.Handle(context.Background(), r); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) due() time.Time {
	h.t.Helper()
	next, err := h.d.RunDue(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return next
}

// records lists the journal as "kind:detail" lines.
func (h *harness) records() []string {
	h.t.Helper()
	var out []string
	_, err := h.d.Journal.Replay(func(r journal.Record) error {
		out = append(out, r.Kind+" "+string(r.Data))
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) count(substr string) int {
	n := 0
	for _, r := range h.records() {
		if strings.Contains(r, substr) {
			n++
		}
	}
	return n
}

func completed(seq string, started, done time.Duration) provider.Rollout {
	return provider.Rollout{Service: api, Revision: "h" + seq, Sequence: seq, State: provider.RolloutComplete,
		StartedAt: t0.Add(started), CompletedAt: t0.Add(done)}
}

func TestCheckpointsRunInTurn(t *testing.T) {
	h := newHarness(t)
	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)

	if next := h.at(2 * time.Minute).due(); !next.Equal(t0.Add(6 * time.Minute)) {
		t.Fatalf("next due %s, want +5 after completion", next)
	}
	h.at(6 * time.Minute).due()
	h.at(16 * time.Minute).due()
	if next := h.at(31 * time.Minute).due(); !next.IsZero() {
		t.Errorf("nothing should be pending, next %s", next)
	}
	if got := h.checker.seen(); got != "2@5m0s 2@15m0s 2@30m0s" {
		t.Errorf("checks %q", got)
	}
	if len(h.checks) != 3 || h.checks[0].Late {
		t.Errorf("OnCheck got %+v", h.checks)
	}

	// Re-reported at start or by a resync: nothing new.
	h.handle(r)
	if n := h.count(`"state":"complete"`); n != 1 {
		t.Errorf("%d rollout records, want 1", n)
	}
}

func TestRestartResumesWithoutRepeatingAndSkipsMissedCheckpoints(t *testing.T) {
	h := newHarness(t)
	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	h.at(6 * time.Minute).due() // +5 checked

	// Down from +6 to +41: +15 and +30 were missed.
	h.restart()
	h.at(41 * time.Minute).handle(r) // the source re-reports the current rollout at start
	h.due()

	if got := h.checker.seen(); got != "2@5m0s 2@30m0s" {
		t.Errorf("checks %q, want +5 then only the latest missed one", got)
	}
	if !h.checks[len(h.checks)-1].Late {
		t.Error("the catch-up check is not marked late")
	}
	if n := h.count("missed while the daemon was down"); n != 1 {
		t.Errorf("%d missed-checkpoint records, want 1 (+15)", n)
	}

	h.restart()
	h.at(50 * time.Minute).due()
	if got := h.checker.seen(); got != "2@5m0s 2@30m0s" {
		t.Errorf("after a second restart: %q", got)
	}
}

func TestCheckpointsDueTogetherBothRun(t *testing.T) {
	// The loop was busy and two checkpoints fell due within a minute of
	// each other: that is not downtime, both run.
	h := newHarness(t)
	h.d.Checkpoints = []time.Duration{5 * time.Minute, 5*time.Minute + 30*time.Second}
	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	h.at(6*time.Minute + 40*time.Second).due()
	if got := h.checker.seen(); got != "2@5m0s 2@5m30s" {
		t.Errorf("checks %q", got)
	}
}

func TestStaleRolloutIsNotChecked(t *testing.T) {
	h := newHarness(t)
	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(3 * time.Hour).handle(r) // first start, long after the rollout
	h.due()
	if got := h.checker.seen(); got != "" {
		t.Errorf("checks %q on a stale rollout", got)
	}
	if n := h.count("stale"); n != 3 {
		t.Errorf("%d stale records, want 3", n)
	}
}

func TestNewerRolloutCancelsPendingCheckpoints(t *testing.T) {
	h := newHarness(t)
	two := completed("2", 0, time.Minute)
	h.source.current = two
	h.at(time.Minute).handle(two)
	h.at(6 * time.Minute).due()

	// Revision 3 starts at +10 and completes at +12: revision 2's +15
	// and +30 are cancelled, and revision 3's before window ends when
	// revision 2 started.
	three := completed("3", 10*time.Minute, 12*time.Minute)
	h.source.current = three
	h.at(12 * time.Minute).handle(three)
	h.at(17 * time.Minute).due()

	if got := h.checker.seen(); got != "2@5m0s 3@5m0s" {
		t.Errorf("checks %q", got)
	}
	if n := h.count("superseded by revision 3"); n != 2 {
		t.Errorf("%d superseded records, want 2", n)
	}
	if c := h.checker.calls[1]; !c.beforeEnd.Equal(two.StartedAt) {
		t.Errorf("revision 3's before window ends at %s, want revision 2's start %s", c.beforeEnd, two.StartedAt)
	}
}

func TestRolloutInProgressSupersedesBeforeItIsReported(t *testing.T) {
	// The source reports rollouts when they end: revision 3, still in
	// progress at +15, is only visible through LastRollout.
	h := newHarness(t)
	two := completed("2", 0, time.Minute)
	h.source.current = two
	h.at(time.Minute).handle(two)
	h.at(6 * time.Minute).due()

	h.source.current = provider.Rollout{Service: api, Sequence: "3", State: provider.RolloutInProgress, StartedAt: t0.Add(10 * time.Minute)}
	h.at(16 * time.Minute).due()
	if got := h.checker.seen(); got != "2@5m0s" {
		t.Errorf("checks %q, want +15 and +30 cancelled", got)
	}
	if n := h.count("superseded by revision 3, in progress"); n != 2 {
		t.Errorf("%d superseded records, want 2", n)
	}
}

func TestFailedThenCompleted(t *testing.T) {
	h := newHarness(t)
	failed := provider.Rollout{Service: api, Revision: "h2", Sequence: "2", State: provider.RolloutFailed,
		StartedAt: t0, FailedAt: t0.Add(10 * time.Minute), FailureReason: "ProgressDeadlineExceeded"}
	h.source.current = failed
	h.at(10 * time.Minute).handle(failed)
	if got := h.checker.seen(); got != "2@0s" {
		t.Fatalf("a failed rollout is checked at once: %q", got)
	}
	h.handle(failed) // re-reported
	h.due()

	// The probe is fixed and the controller finishes the rollout.
	done := completed("2", 0, 20*time.Minute)
	h.source.current = done
	h.at(20 * time.Minute).handle(done)
	h.at(25 * time.Minute).due()
	if got := h.checker.seen(); got != "2@0s 2@5m0s" {
		t.Errorf("checks %q, want the failure check then +5 from the late completion", got)
	}
}

func TestOldFailureFirstSeenAtStartIsNotChecked(t *testing.T) {
	h := newHarness(t)
	failed := provider.Rollout{Service: api, Sequence: "2", State: provider.RolloutFailed, StartedAt: t0, FailedAt: t0.Add(10 * time.Minute)}
	h.at(5 * time.Hour).handle(failed)
	if got := h.checker.seen(); got != "" {
		t.Errorf("checks %q", got)
	}
	if n := h.count(`"state":"failed"`); n != 1 {
		t.Errorf("the failure is still recorded: %d records", n)
	}
}

func TestCheckErrorIsRetried(t *testing.T) {
	h := newHarness(t)
	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	h.checker.fail = true
	if next := h.at(6 * time.Minute).due(); !next.Equal(t0.Add(7 * time.Minute)) {
		t.Errorf("retry at %s, want a minute later", next)
	}
	h.checker.fail = false
	h.at(6*time.Minute + 30*time.Second).due() // backend back, but too early to retry
	if got := h.checker.seen(); got != "" {
		t.Errorf("retried before the delay: %q", got)
	}
	h.at(7 * time.Minute).due()
	if got := h.checker.seen(); got != "2@5m0s" {
		t.Errorf("checks %q", got)
	}
	if n := h.count(KindCheck + " "); n != 1 {
		t.Errorf("%d check records, want 1 (a failed attempt is not recorded)", n)
	}
}

func TestBeforeEndFollowsTheChainOfCloseRollouts(t *testing.T) {
	h := newHarness(t)
	for _, r := range []provider.Rollout{
		{Service: api, Sequence: "1", State: provider.RolloutComplete, StartedAt: t0.Add(-50 * time.Minute), CompletedAt: t0.Add(-49 * time.Minute)},
		{Service: api, Sequence: "2", State: provider.RolloutSuperseded, StartedAt: t0.Add(-25 * time.Minute)},
		{Service: api, Sequence: "3", State: provider.RolloutComplete, StartedAt: t0.Add(-5 * time.Minute), CompletedAt: t0},
	} {
		h.at(r.CompletedAt.Sub(t0)).handle(r)
	}
	if got := h.d.beforeEnd(h.d.services["prod/api"].rollouts["3"]); !got.Equal(t0.Add(-50 * time.Minute)) {
		t.Errorf("before window ends at %s, want revision 1's start (each start under 30 min from the next)", got)
	}
	if got := h.d.beforeEnd(h.d.services["prod/api"].rollouts["1"]); !got.IsZero() {
		t.Errorf("revision 1 follows nothing close: %s", got)
	}
}

func TestBeforeEndTracesLongChainsPastStaleAfter(t *testing.T) {
	for _, every := range []time.Duration{20 * time.Minute, 5 * time.Minute} {
		h := newHarness(t)
		start := t0.Add(-4 * time.Hour)
		var last string
		for i := 0; !start.Add(time.Duration(i) * every).After(t0); i++ {
			at := start.Add(time.Duration(i) * every)
			last = strconv.Itoa(i + 1)
			h.at(at.Add(time.Minute).Sub(t0)).handle(provider.Rollout{Service: api, Sequence: last,
				State: provider.RolloutComplete, StartedAt: at, CompletedAt: at.Add(time.Minute)})
		}
		rs := h.d.services["prod/api"].rollouts[last]
		got := h.d.beforeEnd(rs)
		// Every 5 minutes is 49 rollouts, past keepRollouts: the chain
		// must still reach back beyond stale_after.
		if gap := rs.rollout.StartedAt.Sub(got); gap <= health.DefaultConfig().StaleAfter {
			t.Errorf("every %s: before window ends %s before the last rollout, want past stale_after", every, gap)
		}
	}
}

func TestRunFollowsEventsAndCheckpoints(t *testing.T) {
	j, err := journal.Open(t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	r := provider.Rollout{Service: api, Sequence: "2", State: provider.RolloutComplete, StartedAt: time.Now(), CompletedAt: time.Now()}
	src := &fakeSource{current: r, events: make(chan provider.Rollout, 1)}
	chk := &fakeChecker{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 3)
	d := &Daemon{
		Source: src, Checker: chk, Journal: j,
		Checkpoints: []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond},
		OnCheck:     func(context.Context, Check) { done <- 1 },
	}
	src.events <- r
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()
	for range 3 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("only %q checked", chk.seen())
		}
	}
	cancel()
	if err := <-errc; err != nil {
		t.Errorf("Run: %v", err)
	}
	if got := chk.seen(); got != "2@50ms 2@100ms 2@150ms" {
		t.Errorf("checks %q", got)
	}
}

func TestNewerPrefersStartTimesOverRevisionNumbers(t *testing.T) {
	// Compose numbers no revision: a config-hash made of digits only is
	// a Revision, never compared as a number.
	older := provider.Rollout{Revision: "900000000000aa", StartedAt: t0}
	later := provider.Rollout{Revision: "100000000000bb", StartedAt: t0.Add(time.Minute)}
	if !newer(later, older) || newer(older, later) {
		t.Error("start times must decide")
	}
	// A skipped revision is known by its number only.
	if !newer(provider.Rollout{Sequence: "4"}, provider.Rollout{Sequence: "3", StartedAt: t0}) {
		t.Error("revision numbers decide without a start time")
	}
}

// Compose rollouts have no Sequence: the daemon tells them apart by
// their revision, and records them under its short form.
func TestRolloutsWithoutSequence(t *testing.T) {
	h := newHarness(t)
	compose := func(hash string, started, done time.Duration) provider.Rollout {
		return provider.Rollout{Service: api, Revision: "sha256:" + hash, State: provider.RolloutComplete,
			StartedAt: t0.Add(started), CompletedAt: t0.Add(done)}
	}
	a := compose("aaaaaaaaaaaa1111", 0, time.Minute)
	b := compose("bbbbbbbbbbbb2222", 10*time.Minute, 11*time.Minute)
	h.source.current = a
	h.at(time.Minute).handle(a)
	h.at(6 * time.Minute).due()
	h.source.current = b
	h.at(11 * time.Minute).handle(b)
	h.at(16 * time.Minute).due()
	if got := h.checker.seen(); got != "aaaaaaaaaaaa@5m0s bbbbbbbbbbbb@5m0s" {
		t.Errorf("checks %q", got)
	}
	if n := h.count(`"superseded by revision bbbbbbbbbbbb"`); n != 2 {
		t.Errorf("%d checkpoints of the first rollout superseded by the second, want 2\n%s", n, strings.Join(h.records(), "\n"))
	}
}
