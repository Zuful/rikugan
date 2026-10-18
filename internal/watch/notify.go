package watch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/watch/sweep"
	"github.com/Zuful/rikugan/pkg/provider"
)

// KindNotification records a notification decision in the journal.
const KindNotification = "notification"

// Notification events.
const (
	// EventVerdict notifies a verdict: the first one of a rollout worth
	// a message, or a later one that is worse.
	EventVerdict = "verdict"
	// EventRecovered tells the person a critical woke up that a later
	// check is back below degraded.
	EventRecovered = "recovered"
)

// notifyState is a rollout's notification state (design v3.5 §5). The
// noise policy is one notification per rollout, renotified only when
// the verdict gets worse than the reference.
type notifyState struct {
	// Ref is the level the next verdict is compared with: the last
	// level notified, or the level of a "recovered" message.
	Ref health.Status `json:"ref,omitempty"`
	// CriticalOpen is set after a critical, until a "recovered".
	CriticalOpen bool `json:"critical_open,omitempty"`
}

// decision is what to send after a check, if anything.
type decision struct {
	Event    string
	Severity provider.Severity
}

// decide applies the noise policy to a verdict:
//
//   - degraded is notified as critical unless the reference is already
//     degraded;
//   - after a critical, the first verdict back below degraded (ok or
//     investigate) sends one "recovered" message, whose level becomes
//     the reference: degraded again is a new critical;
//   - "insufficient data" never counts as a recovery: the absence of a
//     measure is not proof that things got better;
//   - investigate is notified as a warning when nothing as bad was
//     notified before;
//   - ok is notified only with notifyOnOK, once per rollout.
func decide(st notifyState, verdict health.Status, notifyOnOK bool) (notifyState, *decision) {
	switch verdict {
	case health.StatusDegraded:
		if st.Ref == health.StatusDegraded {
			return st, nil
		}
		return notifyState{Ref: health.StatusDegraded, CriticalOpen: true},
			&decision{Event: EventVerdict, Severity: provider.SeverityCritical}
	case health.StatusOK, health.StatusInvestigate:
		if st.CriticalOpen {
			return notifyState{Ref: verdict},
				&decision{Event: EventRecovered, Severity: provider.SeverityInfo}
		}
		if verdict == health.StatusInvestigate && rankOf(st.Ref) < rankOf(health.StatusInvestigate) {
			return notifyState{Ref: verdict},
				&decision{Event: EventVerdict, Severity: provider.SeverityWarning}
		}
		if verdict == health.StatusOK && notifyOnOK && st.Ref == "" {
			return notifyState{Ref: verdict},
				&decision{Event: EventVerdict, Severity: provider.SeverityInfo}
		}
	}
	return st, nil
}

func rankOf(s health.Status) int {
	switch s {
	case health.StatusDegraded:
		return 3
	case health.StatusInvestigate:
		return 2
	case health.StatusOK:
		return 1
	}
	return 0
}

// Notifier sends the daemon's messages to the team.
type Notifier struct {
	// Channels receive every message; an empty list records decisions
	// without sending anything.
	Channels []provider.CommunicationProvider
	// Alerts, when set, is consulted before sending: an alert already
	// firing for the service is mentioned in the message.
	Alerts provider.AlertsProvider
	// NotifyOnOK (notify_on_ok) also notifies OK verdicts.
	NotifyOnOK bool
	Logger     *slog.Logger
	// Retries per channel after a failed send, and the pause between
	// attempts.
	Retries int
	Backoff time.Duration
}

func (n *Notifier) logger() *slog.Logger {
	if n.Logger != nil {
		return n.Logger
	}
	return slog.Default()
}

// Send delivers msg to every channel, retrying each a few times. It
// returns the channels that took it.
func (n *Notifier) Send(ctx context.Context, msg provider.TeamMessage) []string {
	var delivered []string
	for _, ch := range n.Channels {
		var err error
		for attempt := 0; attempt <= n.Retries; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return delivered
				case <-time.After(n.Backoff):
				}
			}
			if err = ch.PostMessage(ctx, msg); err == nil {
				delivered = append(delivered, ch.Name())
				break
			}
		}
		if err != nil {
			n.logger().Error("notification not delivered", slog.String("channel", ch.Name()),
				slog.String("title", msg.Title), slog.String("error", err.Error()))
		}
	}
	return delivered
}

// openAlerts returns the titles of the alerts firing for the service.
// An error is reported in the message rather than holding it back.
func (n *Notifier) openAlerts(ctx context.Context, svc provider.Service) (titles []string, note string) {
	if n.Alerts == nil {
		return nil, ""
	}
	alerts, err := n.Alerts.ListActiveAlerts(ctx, svc.Name)
	if err != nil {
		n.logger().Warn("alerts lookup failed", slog.String("service", serviceKey(svc)), slog.String("error", err.Error()))
		return nil, "Open alerts could not be checked: " + err.Error()
	}
	for _, a := range alerts {
		if a.State == "" || a.State == "firing" || a.State == "acknowledged" {
			titles = append(titles, a.Title)
		}
	}
	return titles, ""
}

// message renders a decision as a team message.
func (n *Notifier) message(ctx context.Context, c Check, d decision) provider.TeamMessage {
	r := c.Rollout
	where := "+" + c.Checkpoint.String()
	if c.Checkpoint == FailureCheckpoint {
		where = "rollout failure"
	}
	var title string
	switch d.Event {
	case EventRecovered:
		title = fmt.Sprintf("%s: recovered, revision %s now %s (%s)", serviceKey(r.Service), r.Sequence, c.Report.Verdict, where)
	default:
		title = fmt.Sprintf("%s: revision %s %s (%s)", serviceKey(r.Service), r.Sequence, c.Report.Verdict, where)
	}

	var b strings.Builder
	if r.Rollback {
		b.WriteString("Rollback.\n")
	}
	if c.Late {
		b.WriteString("Late check: the daemon was down at the checkpoint.\n")
	}
	n.writeFindings(ctx, &b, r.Service, c.Report)
	return provider.TeamMessage{Severity: d.Severity, Title: title, Body: strings.TrimSpace(b.String())}
}

// writeFindings appends the signals that are not ok, the notes, and the
// alerts already firing for the service.
func (n *Notifier) writeFindings(ctx context.Context, b *strings.Builder, svc provider.Service, rep health.Report) {
	for _, s := range rep.Signals {
		if s.Status == health.StatusDegraded || s.Status == health.StatusInvestigate {
			fmt.Fprintf(b, "- %s: %s, %s\n", s.Name, s.Status, s.Reason)
		}
	}
	for _, note := range rep.Notes {
		fmt.Fprintf(b, "- %s\n", note)
	}
	titles, alertNote := n.openAlerts(ctx, svc)
	if len(titles) > 0 {
		fmt.Fprintf(b, "Already firing for this service (%d): %s\n", len(titles), strings.Join(titles, "; "))
	}
	if alertNote != "" {
		b.WriteString(alertNote + "\n")
	}
}

// sweepMessage renders a sweep notification.
func (n *Notifier) sweepMessage(ctx context.Context, svc provider.Service, rep health.Report, note sweep.Notification) provider.TeamMessage {
	key := serviceKey(svc)
	var title string
	switch note.Event {
	case sweep.EventIncident:
		title = fmt.Sprintf("%s: degraded (periodic sweep)", key)
	case sweep.EventPersistentWarning:
		title = fmt.Sprintf("%s: investigate for %d sweeps in a row", key, note.Streak)
	case sweep.EventResolved:
		title = fmt.Sprintf("%s: resolved, two sweeps ok in a row", key)
	default:
		title = fmt.Sprintf("%s: investigate (periodic sweep)", key)
	}
	var b strings.Builder
	if note.Event != sweep.EventResolved {
		n.writeFindings(ctx, &b, svc, rep)
	}
	return provider.TeamMessage{Severity: note.Severity, Title: title, Body: strings.TrimSpace(b.String())}
}
