package watch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

var badErrorRate = health.InvalidTemplate{Name: "error_rate", Template: `sum(rate(http_requests_total{code=~"5.."}[1m]) or`, Err: "invalid query: bad_data: parse error"}

// A template the backend refuses is warned about once, whichever
// service meets it and across restarts; until then E-rate stays
// unevaluated without anyone knowing.
func TestInvalidTemplateIsWarnedOnce(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram"}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.checker.invalid = []health.InvalidTemplate{badErrorRate}

	r := completed("2", 0, time.Minute)
	h.source.current = r
	h.at(time.Minute).handle(r)
	h.at(6 * time.Minute).due()
	h.at(16 * time.Minute).due()
	other := r
	other.Service = provider.Service{Namespace: "shop", Name: "web"}
	h.d.check(context.Background(), h.d.rolloutState(other), 5*time.Minute, false)

	want := "[warning] rikugan: template error_rate invalid"
	if got := tg.titles(); got != want {
		t.Fatalf("messages:\n%s\nwant:\n%s", got, want)
	}
	body := tg.msgs[0].Body
	for _, part := range []string{"the error rate is not evaluated", "First seen on prod/api", badErrorRate.Template, badErrorRate.Err} {
		if !strings.Contains(body, part) {
			t.Errorf("body lacks %q:\n%s", part, body)
		}
	}

	h.at(20 * time.Minute).restart()
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.at(31 * time.Minute).due()
	if got := tg.titles(); got != want {
		t.Errorf("warned again after a restart:\n%s", got)
	}

	// Edited but still wrong: a new template, a new warning.
	fixedBadly := badErrorRate
	fixedBadly.Template += " vector(0)"
	h.d.warnInvalidTemplates(context.Background(), r.Service, []health.InvalidTemplate{fixedBadly})
	if got := tg.titles(); got != want+"\n"+want {
		t.Errorf("an edited template is not warned about:\n%s", got)
	}
}

// No channel takes the warning: nothing is recorded, and the next check
// meeting the template tries again.
func TestInvalidTemplateWarningRetriedWhenUndelivered(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram", fails: -1}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	svc := provider.Service{Namespace: "shop", Name: "api"}
	h.d.warnInvalidTemplates(context.Background(), svc, []health.InvalidTemplate{badErrorRate})
	if n := h.count(KindInvalidTemplate); n != 0 {
		t.Fatalf("%d records for an undelivered warning", n)
	}
	tg.mu.Lock()
	tg.fails = 0
	tg.mu.Unlock()
	h.d.warnInvalidTemplates(context.Background(), svc, []health.InvalidTemplate{badErrorRate})
	if got := tg.titles(); got != "[warning] rikugan: template error_rate invalid" {
		t.Errorf("messages: %q", got)
	}
	if n := h.count(KindInvalidTemplate); n != 1 {
		t.Errorf("%d records, want 1", n)
	}
}

// At start, every service's templates are dry-run; the invalid ones are
// warned about before any rollout needs them, and a backend that cannot
// answer is only logged.
func TestTemplatesValidatedAtStart(t *testing.T) {
	h := newHarness(t)
	tg := &fakeChannel{name: "telegram"}
	h.d.Notifier = &Notifier{Channels: []provider.CommunicationProvider{tg}}
	h.d.ValidateTemplates = func(context.Context) []TemplateCheck {
		return []TemplateCheck{
			{Service: provider.Service{Namespace: "shop", Name: "api"}, Invalid: []health.InvalidTemplate{badErrorRate}},
			{Service: provider.Service{Namespace: "shop", Name: "web"}, Invalid: []health.InvalidTemplate{badErrorRate}},
			{Service: provider.Service{Namespace: "shop", Name: "db"}, Err: errors.New("connection refused")},
		}
	}
	h.source.events = make(chan provider.Rollout)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- h.d.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for tg.titles() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := tg.titles(); got != "[warning] rikugan: template error_rate invalid" {
		t.Errorf("messages at start: %q", got)
	}
}
