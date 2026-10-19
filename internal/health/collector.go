package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"text/template"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Templates are the metric queries behind each signal, as Go templates
// over {{.Namespace}}, {{.Service}} and {{.Revision}}, and {{.Window}}
// for LatencyWindowP95. GetMetric takes
// a raw query, so templates are written in the dialect of the metrics
// backend — PromQL for the Prometheus provider. Each query must return
// a single aggregated series. An empty template disables its signal.
type Templates struct {
	RequestRate string `json:"request_rate"`
	ErrorRate   string `json:"error_rate"`
	LatencyP95  string `json:"latency_p95"`
	// LatencyWindowP95 is the p95 of all the requests of a window ending
	// at the query time, {{.Window}} long (a PromQL duration). It is
	// evaluated once per window; LatencyP95 is the per-minute series the
	// median method reads.
	LatencyWindowP95 string `json:"latency_window_p95"`
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
}

// DefaultTemplates follow common conventions: the Prometheus client
// libraries' http_requests_total / http_request_duration_seconds with
// namespace and service labels, and cAdvisor container metrics for pods
// named after their Deployment. Resource queries average over pods, so
// they are per replica: a scale-out does not look like saturation.
func DefaultTemplates() Templates {
	const sel = `namespace="{{.Namespace}}",service="{{.Service}}"`
	const pods = `namespace="{{.Namespace}}",pod=~"{{.Service}}-[a-z0-9]+-[a-z0-9]+",container!="",container!="POD"`
	return Templates{
		RequestRate: `sum(rate(http_requests_total{` + sel + `}[1m]))`,
		// "or vector(0)": without any 5xx yet, the 5xx series does not
		// exist and the sum is empty; zero errors is what that means.
		ErrorRate:        `sum(rate(http_requests_total{` + sel + `,code=~"5.."}[1m])) or vector(0)`,
		LatencyP95:       `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{` + sel + `}[1m])))`,
		LatencyWindowP95: `histogram_quantile(0.95, sum by (le) (increase(http_request_duration_seconds_bucket{` + sel + `}[{{.Window}}])))`,
		CPU:              `avg(sum by (pod) (rate(container_cpu_usage_seconds_total{` + pods + `}[1m])))`,
		Memory:           `avg(sum by (pod) (container_memory_working_set_bytes{` + pods + `}))`,
	}
}

// Collector gathers a check's inputs through the concept interfaces and
// runs Evaluate on them. It never talks to a backend client directly.
type Collector struct {
	Metrics       provider.MetricsProvider
	Orchestration provider.OrchestrationProvider
	Deployments   provider.DeploymentSource
	// Settings resolves the thresholds and templates of a service.
	Settings func(svc provider.Service) (Config, Templates)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// seriesStep is the resolution of every series the engine reads.
const seriesStep = time.Minute

// seasonalOffset is how far back the seasonality baseline sits.
const seasonalOffset = 7 * 24 * time.Hour

// Check evaluates the service's last rollout at the latest checkpoint
// it has reached. Metric errors are not fatal: the affected signal is
// reported as not evaluated and the error is noted in the report, so one
// missing metric does not hide the others.
func (c *Collector) Check(ctx context.Context, svc provider.Service) (Report, error) {
	rollout, err := c.Deployments.LastRollout(ctx, svc)
	if err != nil {
		return Report{}, fmt.Errorf("last rollout of %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	r, err := c.CheckRollout(ctx, rollout, CheckOptions{})
	// A failing metrics backend still leaves a report, which says so:
	// the tool shows what it could read rather than an error.
	if be := (*BackendError)(nil); errors.As(err, &be) && be.Kind == BackendMetrics {
		return r, nil
	}
	return r, err
}

// CheckOptions narrows what CheckRollout evaluates. The zero value
// evaluates the latest checkpoint reached, with the usual before window.
type CheckOptions struct {
	// Checkpoint picks the checkpoint to evaluate (Inputs.Checkpoint).
	Checkpoint time.Duration
	// BeforeEnd ends the before window early (Inputs.BeforeEnd).
	BeforeEnd time.Time
}

// CheckRollout evaluates a given rollout, as reported by a
// DeploymentSource. The instance signals are always read as they are
// now.
func (c *Collector) CheckRollout(ctx context.Context, rollout provider.Rollout, opts CheckOptions) (Report, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	svc := rollout.Service
	cfg, tpl := DefaultConfig(), DefaultTemplates()
	if c.Settings != nil {
		cfg, tpl = c.Settings(svc)
	}

	instances, err := c.Orchestration.GetRevisionInstances(ctx, svc, rollout.Revision)
	if err != nil {
		return Report{}, c.orchestrationError(fmt.Errorf("instances of %s/%s revision %s: %w", svc.Namespace, svc.Name, rollout.Revision, err))
	}
	in := Inputs{Rollout: rollout, Now: now, Instances: instances, Checkpoint: opts.Checkpoint, BeforeEnd: opts.BeforeEnd}

	// Metrics only matter when there is a verdict to compute.
	var notes []string
	var metricsErr error
	inv := &invalidTemplates{}
	if rollout.State == provider.RolloutComplete {
		cp, outcome := SelectCheckpoint(cfg, rollout.CompletedAt, now)
		if opts.Checkpoint > 0 && now.Sub(rollout.CompletedAt) >= opts.Checkpoint {
			cp, outcome = opts.Checkpoint, OutcomeEvaluated
		}
		if outcome == OutcomeEvaluated {
			vars := templateVars{Namespace: svc.Namespace, Service: svc.Name, Revision: rollout.Revision}
			beforeEnd := rollout.StartedAt
			if !opts.BeforeEnd.IsZero() && opts.BeforeEnd.Before(beforeEnd) {
				beforeEnd = opts.BeforeEnd
			}
			from := beforeEnd.Add(-cfg.BeforeWindow)
			to := rollout.CompletedAt.Add(cp)
			in.Current, notes, metricsErr = c.fetch(ctx, tpl, vars, from, to, 0, inv)
			if metricsErr == nil {
				seasonal, _, _ := c.fetch(ctx, Templates{RequestRate: tpl.RequestRate}, vars, from, to, seasonalOffset, inv)
				if len(seasonal.RequestRate) > 0 {
					in.Seasonal = &seasonal
				}
			}
			if metricsErr == nil && cfg.LatencyMethod != LatencyMedian && tpl.LatencyWindowP95 != "" {
				lw := &WindowP95{}
				lw.Before, lw.HasBefore = c.windowP95(ctx, tpl, vars, beforeEnd, cfg.BeforeWindow, 0, &notes, inv)
				lw.After, lw.HasAfter = c.windowP95(ctx, tpl, vars, to, cp, 0, &notes, inv)
				in.LatencyWindow = lw
			}
		}
	}

	r := Evaluate(cfg, in)
	r.InvalidTemplates = inv.list
	if metricsErr != nil {
		metricsUnavailable(&r, c.Metrics.Name(), metricsErr)
		r.Notes = append(r.Notes, inv.notes()...)
		return r, &BackendError{Kind: BackendMetrics, Name: c.Metrics.Name(), Err: metricsErr}
	}
	r.Notes = append(r.Notes, notes...)
	r.Notes = append(r.Notes, inv.notes()...)
	return r, nil
}

// TemplateProbe is one query template sent as a dry run.
type TemplateProbe struct {
	// Name is the template's settings key: request_rate, error_rate…
	Name     string
	Template string
	// Invalid is set when the template does not render or the backend
	// refused it; Err says why.
	Invalid bool
	Err     string
	// Series is how many series the query returned: 0 is a template
	// that selects nothing, often a wrong metric or label name.
	Series int
}

// ProbeTemplates sends each of the service's query templates as a dry
// run over the last span, and says what each one returned. An error
// means the backend itself failed every query: nothing was probed.
func (c *Collector) ProbeTemplates(ctx context.Context, svc provider.Service, span time.Duration) ([]TemplateProbe, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	_, tpl := DefaultConfig(), DefaultTemplates()
	if c.Settings != nil {
		_, tpl = c.Settings(svc)
	}
	vars := templateVars{Namespace: svc.Namespace, Service: svc.Name, Window: fmt.Sprintf("%ds", int(span.Seconds()))}
	var out []TemplateProbe
	sent, failed := 0, 0
	var firstErr error
	for _, q := range []struct{ key, tpl string }{
		{"request_rate", tpl.RequestRate},
		{"error_rate", tpl.ErrorRate},
		{"latency_p95", tpl.LatencyP95},
		{"latency_window_p95", tpl.LatencyWindowP95},
		{"cpu", tpl.CPU},
		{"memory", tpl.Memory},
	} {
		if q.tpl == "" {
			continue
		}
		probe := TemplateProbe{Name: q.key, Template: q.tpl}
		inv := &invalidTemplates{}
		query, err := render(q.tpl, vars)
		if inv.add(q.key, q.tpl, err) {
			probe.Invalid, probe.Err = true, err.Error()
			out = append(out, probe)
			continue
		}
		if err != nil {
			continue // a name render refuses: not the template's fault
		}
		samples, err := c.Metrics.GetMetric(ctx, query, now.Add(-span), now, seriesStep)
		if inv.add(q.key, q.tpl, err) {
			probe.Invalid, probe.Err = true, err.Error()
			out = append(out, probe)
			continue
		}
		sent++
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_, probe.Series = singleSeries(samples, 0)
		out = append(out, probe)
	}
	if sent > 0 && failed == sent {
		return nil, &BackendError{Kind: BackendMetrics, Name: c.Metrics.Name(), Err: firstErr}
	}
	return out, nil
}

// ValidateTemplates sends each of the service's query templates as a
// dry run over the last few minutes, and returns those that cannot
// work as written: they do not render, or the backend refuses them. An
// error means the backend itself failed every query: nothing was
// validated. An empty result is not checked: a service may have served
// no 5xx, or no request at all, lately.
func (c *Collector) ValidateTemplates(ctx context.Context, svc provider.Service) ([]InvalidTemplate, error) {
	probes, err := c.ProbeTemplates(ctx, svc, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	var out []InvalidTemplate
	for _, p := range probes {
		if p.Invalid {
			out = append(out, InvalidTemplate{Name: p.Name, Template: p.Template, Err: p.Err})
		}
	}
	return out, nil
}

func (c *Collector) orchestrationError(err error) error {
	return &BackendError{Kind: BackendOrchestration, Name: c.Orchestration.Name(), Err: err}
}

type templateVars struct{ Namespace, Service, Revision, Window string }

// safeName matches Kubernetes names and Compose service/project names.
// Template variables are interpolated into queries, so anything else is
// refused rather than escaped.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.-]*[a-zA-Z0-9])?$`)

// windowP95 evaluates LatencyWindowP95 for the window of length w
// ending at end (shifted back by offset). A query error is a note; no
// value (no requests in the window, NaN) is not evaluated.
func (c *Collector) windowP95(ctx context.Context, tpl Templates, vars templateVars, end time.Time, w, offset time.Duration, notes *[]string, inv *invalidTemplates) (float64, bool) {
	vars.Window = fmt.Sprintf("%ds", int(w.Seconds()))
	query, err := render(tpl.LatencyWindowP95, vars)
	if err != nil {
		if !inv.add("latency_window_p95", tpl.LatencyWindowP95, err) {
			*notes = append(*notes, fmt.Sprintf("latency window p95 query: %v", err))
		}
		return 0, false
	}
	at := end.Add(-offset)
	samples, err := c.Metrics.GetMetric(ctx, query, at, at, seriesStep)
	if inv.add("latency_window_p95", tpl.LatencyWindowP95, err) {
		return 0, false
	}
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("latency window p95 query failed: %v", err))
		return 0, false
	}
	series, n := singleSeries(samples, 0)
	if n > 1 {
		*notes = append(*notes, fmt.Sprintf("latency window p95 query returned %d series, expected one aggregated series; signal skipped", n))
		return 0, false
	}
	if len(series) == 0 {
		return 0, false
	}
	v := series[len(series)-1].Value
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// fetch runs every non-empty template over [from, to], shifted back by
// offset. Returned samples are shifted forward by the same offset so
// they line up with the current window. A failing query is a note; the
// error is only set when every query sent failed: the metrics backend
// itself is failing, not one template.
func (c *Collector) fetch(ctx context.Context, tpl Templates, vars templateVars, from, to time.Time, offset time.Duration, inv *invalidTemplates) (Signals, []string, error) {
	var sig Signals
	var notes []string
	sent, failed := 0, 0
	var firstErr error
	for _, q := range []struct {
		name, key string
		tpl       string
		dst       *Series
	}{
		{"request rate", "request_rate", tpl.RequestRate, &sig.RequestRate},
		{"error rate", "error_rate", tpl.ErrorRate, &sig.ErrorRate},
		{"latency p95", "latency_p95", tpl.LatencyP95, &sig.LatencyP95},
		{"CPU", "cpu", tpl.CPU, &sig.CPU},
		{"memory", "memory", tpl.Memory, &sig.Memory},
	} {
		if q.tpl == "" {
			continue
		}
		query, err := render(q.tpl, vars)
		if err != nil {
			if !inv.add(q.key, q.tpl, err) {
				notes = append(notes, fmt.Sprintf("%s query: %v", q.name, err))
			}
			continue
		}
		samples, err := c.Metrics.GetMetric(ctx, query, from.Add(-offset), to.Add(-offset), seriesStep)
		if inv.add(q.key, q.tpl, err) {
			continue // the backend answered: it refused the query
		}
		sent++
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			notes = append(notes, fmt.Sprintf("%s query failed: %v", q.name, err))
			continue
		}
		series, n := singleSeries(samples, offset)
		if n > 1 {
			notes = append(notes, fmt.Sprintf("%s query returned %d series, expected one aggregated series; signal skipped", q.name, n))
			continue
		}
		*q.dst = series
	}
	if sent > 0 && failed == sent {
		return sig, notes, firstErr
	}
	return sig, notes, nil
}

func render(tpl string, vars templateVars) (string, error) {
	for _, v := range []string{vars.Namespace, vars.Service} {
		if !safeName.MatchString(v) {
			return "", fmt.Errorf("refusing to interpolate %q into a query", v)
		}
	}
	if vars.Revision != "" && !safeName.MatchString(vars.Revision) {
		return "", fmt.Errorf("refusing to interpolate %q into a query", vars.Revision)
	}
	t, err := template.New("q").Option("missingkey=error").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errTemplate, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("%w: %w", errTemplate, err)
	}
	return b.String(), nil
}

// errTemplate marks a template that does not parse or execute.
var errTemplate = errors.New("template does not render")

// invalidTemplates collects the templates of one check that cannot
// work as written, once each.
type invalidTemplates struct{ list []InvalidTemplate }

// add records err against the template if it says the template itself
// is wrong: it does not render, or the backend refused the query. It
// reports whether it did.
func (inv *invalidTemplates) add(key, tpl string, err error) bool {
	if !errors.Is(err, errTemplate) && !errors.Is(err, provider.ErrInvalidQuery) {
		return false
	}
	for _, t := range inv.list {
		if t.Name == key {
			return true
		}
	}
	inv.list = append(inv.list, InvalidTemplate{Name: key, Template: tpl, Err: err.Error()})
	return true
}

// notes describes the invalid templates for a report.
func (inv *invalidTemplates) notes() []string {
	var out []string
	for _, t := range inv.list {
		out = append(out, fmt.Sprintf("template %s invalid: %s", t.Name, t.Err))
	}
	return out
}

// singleSeries returns the samples of an aggregated query in time
// order, shifted forward by offset, along with how many distinct series
// the query returned. More than one means the template does not
// aggregate, and there is no sound way to merge, say, two latencies.
func singleSeries(samples []provider.MetricSample, offset time.Duration) (Series, int) {
	keys := map[string]bool{}
	out := make(Series, 0, len(samples))
	for _, s := range samples {
		keys[fmt.Sprint(s.Labels)] = true
		out = append(out, provider.MetricSample{Time: s.Time.Add(offset), Value: s.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, len(keys)
}

// PreviousSweep is what a sweep keeps for the next one: when it ran and
// each instance's restart count then.
type PreviousSweep struct {
	At       time.Time
	Restarts map[string]int // by instance name
}

// CheckSweep evaluates a service for the periodic sweep and returns the
// restart counts to keep for the next one. prev is nil for the first
// sweep of a service.
//
// Restarts are counted per instance since prev (design §6): the delta
// for an instance seen then, the whole count for an instance created
// since, nothing for one that disappeared. Terminations are kept only
// when they happened after prev; on the first sweep, only those within
// the sweep window, the one dated evidence of recent restarts.
func (c *Collector) CheckSweep(ctx context.Context, svc provider.Service, prev *PreviousSweep) (Report, map[string]int, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	cfg, tpl := DefaultConfig(), DefaultTemplates()
	if c.Settings != nil {
		cfg, tpl = c.Settings(svc)
	}

	rollout, err := c.Deployments.LastRollout(ctx, svc)
	if err != nil {
		return Report{}, nil, c.orchestrationError(fmt.Errorf("current revision of %s/%s: %w", svc.Namespace, svc.Name, err))
	}
	instances, err := c.Orchestration.GetRevisionInstances(ctx, svc, rollout.Revision)
	if err != nil {
		return Report{}, nil, c.orchestrationError(fmt.Errorf("instances of %s/%s: %w", svc.Namespace, svc.Name, err))
	}
	counts := make(map[string]int, len(instances))
	since := now.Add(-SweepWindow)
	if prev != nil {
		since = prev.At
	}
	for i, inst := range instances {
		counts[inst.Name] = inst.RestartCount
		if prev != nil {
			if before, seen := prev.Restarts[inst.Name]; seen {
				instances[i].RestartCount = max(inst.RestartCount-before, 0)
			}
		}
		var recent []provider.Termination
		for _, t := range inst.Terminations {
			if t.Time.After(since) {
				recent = append(recent, t)
			}
		}
		instances[i].Terminations = recent
	}

	vars := templateVars{Namespace: svc.Namespace, Service: svc.Name, Revision: rollout.Revision}
	from := now.Add(-cfg.LeakSpan - 15*time.Minute)
	inv := &invalidTemplates{}
	current, notes, metricsErr := c.fetch(ctx, tpl, vars, from, now, 0, inv)
	var baseline *Signals
	var age time.Duration
	for _, offset := range []time.Duration{7 * 24 * time.Hour, 24 * time.Hour} {
		if metricsErr != nil {
			break
		}
		b, _, _ := c.fetch(ctx, tpl, vars, now.Add(-SweepWindow), now, offset, inv)
		if len(b.RequestRate) > 0 {
			baseline, age = &b, offset
			break
		}
	}

	var lw *WindowP95
	if baseline != nil && cfg.LatencyMethod != LatencyMedian && tpl.LatencyWindowP95 != "" {
		lw = &WindowP95{}
		lw.Before, lw.HasBefore = c.windowP95(ctx, tpl, vars, now, SweepWindow, age, &notes, inv)
		lw.After, lw.HasAfter = c.windowP95(ctx, tpl, vars, now, SweepWindow, 0, &notes, inv)
	}
	r := EvaluateSweep(cfg, SweepInputs{
		Now: now, Current: current, Baseline: baseline, BaselineAge: age,
		Instances: instances, FirstSweep: prev == nil, LatencyWindow: lw,
	})
	r.Rollout = rollout
	r.InvalidTemplates = inv.list
	if metricsErr != nil {
		metricsUnavailable(&r, c.Metrics.Name(), metricsErr)
		r.Notes = append(r.Notes, inv.notes()...)
		return r, counts, &BackendError{Kind: BackendMetrics, Name: c.Metrics.Name(), Err: metricsErr}
	}
	r.Notes = append(r.Notes, notes...)
	r.Notes = append(r.Notes, inv.notes()...)
	return r, counts, nil
}
