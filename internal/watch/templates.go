package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Zuful/rikugan/internal/health"
	"github.com/Zuful/rikugan/internal/watch/journal"
	"github.com/Zuful/rikugan/pkg/provider"
)

// KindInvalidTemplate records the warning sent about a query template
// the metrics backend refuses, so that it is sent once, restarts
// included.
const KindInvalidTemplate = "invalid_template"

type invalidTemplateRecord struct {
	Name      string   `json:"name"`
	Template  string   `json:"template"`
	Error     string   `json:"error"`
	Service   string   `json:"service"`
	Delivered []string `json:"delivered,omitempty"`
}

// TemplateCheck is the outcome of validating one service's templates.
type TemplateCheck struct {
	Service provider.Service
	Invalid []health.InvalidTemplate
	// Err is set when the templates could not be validated: the metrics
	// backend is failing, which the checks report on their own.
	Err error
}

// templateSignals says what each template feeds, for the warning.
var templateSignals = map[string]string{
	"request_rate":       "traffic, and latency and error rate (the volume guard needs the request count)",
	"error_rate":         "the error rate",
	"latency_p95":        "latency (median method)",
	"latency_window_p95": "latency",
	"cpu":                "CPU saturation",
	"memory":             "memory saturation and leaks",
}

func invalidKey(name, tpl string) string { return name + "\x00" + tpl }

func (d *Daemon) replayInvalidTemplate(data []byte) {
	var rec invalidTemplateRecord
	if json.Unmarshal(data, &rec) == nil && rec.Name != "" {
		d.invalidWarned[invalidKey(rec.Name, rec.Template)] = true
	}
}

// warnInvalidTemplates tells the team, once per template, that the
// metrics backend refuses it: the signals it feeds would otherwise stay
// unevaluated without anyone knowing, and an OK on the remaining ones
// would hide it. The key is the template's text, shared by every
// service that uses it, so a fixed template that breaks again warns
// again. When no channel takes the message, nothing is recorded and the
// next check that meets the template tries again.
func (d *Daemon) warnInvalidTemplates(ctx context.Context, svc provider.Service, list []health.InvalidTemplate) {
	if d.invalidWarned == nil {
		d.invalidWarned = map[string]bool{}
	}
	for _, t := range list {
		key := invalidKey(t.Name, t.Template)
		if d.invalidWarned[key] {
			continue
		}
		d.logger().Error("invalid query template: the signals it feeds are not evaluated",
			slog.String("template", t.Name), slog.String("service", serviceKey(svc)), slog.String("error", t.Err))
		rec := invalidTemplateRecord{Name: t.Name, Template: t.Template, Error: t.Err, Service: serviceKey(svc)}
		if d.Notifier != nil && len(d.Notifier.Channels) > 0 {
			what := templateSignals[t.Name]
			if what == "" {
				what = "the signal it feeds"
			}
			rec.Delivered = d.Notifier.Send(ctx, provider.TeamMessage{
				Severity: provider.SeverityWarning,
				Title:    fmt.Sprintf("rikugan: template %s invalid", t.Name),
				Body: fmt.Sprintf("The metrics backend refuses this query, so %s is not evaluated for any service using it until it is fixed (RIKUGAN_HEALTH_CONFIG). First seen on %s.\nTemplate: %s\nError: %s",
					what, serviceKey(svc), t.Template, t.Err),
			})
			if len(rec.Delivered) == 0 {
				continue
			}
		}
		data, err := json.Marshal(rec)
		if err == nil {
			err = d.Journal.Append(journal.Record{Time: d.now(), Kind: KindInvalidTemplate, Service: serviceKey(svc), Data: data})
		}
		if err != nil {
			d.logger().Error("cannot record an invalid template warning", slog.String("error", err.Error()))
			continue
		}
		d.invalidWarned[key] = true
	}
}

// validateTemplates runs ValidateTemplates once, at start: a template
// broken from the outset is reported before any rollout needs it.
func (d *Daemon) validateTemplates(ctx context.Context) {
	if d.ValidateTemplates == nil {
		return
	}
	checks := d.ValidateTemplates(ctx)
	invalid := 0
	for _, tc := range checks {
		if tc.Err != nil {
			d.logger().Warn("query templates not validated", slog.String("service", serviceKey(tc.Service)), slog.String("error", tc.Err.Error()))
			continue
		}
		invalid += len(tc.Invalid)
		d.warnInvalidTemplates(ctx, tc.Service, tc.Invalid)
	}
	d.logger().Info("query templates validated", slog.Int("services", len(checks)), slog.Int("invalid", invalid))
}
