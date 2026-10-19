package watch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/pkg/provider"
)

// outage is a backend the daemon cannot read, since its first failure
// with no success since.
type outage struct {
	name   string
	since  time.Time
	err    string
	warned bool
}

// backendFailed records a failure of a backend. Once the outage has
// lasted past limit (stale_after), or a check is given up because of
// it, the team is warned, once per outage: a blind daemon must be an
// event, not a silence. Outages are kept in memory; a restart starts
// the clock again.
func (d *Daemon) backendFailed(ctx context.Context, be *health.BackendError, limit time.Duration) {
	if d.outages == nil {
		d.outages = map[string]*outage{}
	}
	o := d.outages[be.Kind]
	if o == nil {
		o = &outage{name: be.Name, since: d.now()}
		d.outages[be.Kind] = o
	}
	o.err = be.Err.Error()
	if d.now().Sub(o.since) > limit {
		d.warnOutage(ctx, be.Kind)
	}
}

// warnOutage tells the team, once per outage, that a backend has been
// failing long enough for checks to be given up.
func (d *Daemon) warnOutage(ctx context.Context, kind string) {
	o := d.outages[kind]
	if o == nil || o.warned {
		return
	}
	d.logger().Error("backend failing past stale_after: checks are not evaluated",
		slog.String("backend", o.name), slog.Time("since", o.since), slog.String("error", o.err))
	if d.Notifier == nil {
		o.warned = true
		return
	}
	body := "Rollout checks and sweeps cannot be evaluated until it is back. Last error: " + o.err
	if kind == health.BackendMetrics {
		body = "Only restarts and availability are evaluated until it is back; latency, error rate, saturation and traffic are not. Last error: " + o.err
	}
	msg := provider.TeamMessage{
		Severity: provider.SeverityWarning,
		Title:    fmt.Sprintf("rikugan cannot evaluate: %s backend %s failing since %s", kind, o.name, o.since.UTC().Format("2006-01-02 15:04Z")),
		Body:     body,
	}
	o.warned = len(d.Notifier.Channels) == 0 || len(d.Notifier.Send(ctx, msg)) > 0
}

// backendOK closes an outage of that kind, if any; one the team was
// warned about gets an "evaluating again" message.
func (d *Daemon) backendOK(ctx context.Context, kind string) {
	o := d.outages[kind]
	if o == nil {
		return
	}
	delete(d.outages, kind)
	if !o.warned || d.Notifier == nil {
		return
	}
	d.Notifier.Send(ctx, provider.TeamMessage{
		Severity: provider.SeverityInfo,
		Title:    fmt.Sprintf("rikugan evaluates again: %s backend %s back after %s", kind, o.name, d.now().Sub(o.since).Round(time.Minute)),
	})
}

// givenUp is the reason recorded for checks that kept failing until
// stale_after, past which no checkpoint is evaluated anyway.
func (rs *rolloutState) givenUp() string {
	return "not evaluated, given up past stale_after: " + rs.failing
}

// missed is the reason recorded for an overdue checkpoint skipped
// because a later one is due: the daemon was down, or its checks were
// failing.
func (rs *rolloutState) missed() string {
	if rs.failing != "" {
		return "not evaluated, the next checkpoint came while checks were failing: " + rs.failing
	}
	return "missed while the daemon was down"
}
