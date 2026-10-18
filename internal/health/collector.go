package health

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"text/template"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Templates are the metric queries behind each signal, as Go templates
// over {{.Namespace}}, {{.Service}} and {{.Revision}}. GetMetric takes
// a raw query, so templates are written in the dialect of the metrics
// backend — PromQL for the Prometheus provider. Each query must return
// a single aggregated series. An empty template disables its signal.
type Templates struct {
	RequestRate string `json:"request_rate"`
	ErrorRate   string `json:"error_rate"`
	LatencyP95  string `json:"latency_p95"`
	CPU         string `json:"cpu"`
	Memory      string `json:"memory"`
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
		ErrorRate:   `sum(rate(http_requests_total{` + sel + `,code=~"5.."}[1m]))`,
		LatencyP95:  `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{` + sel + `}[1m])))`,
		CPU:         `avg(sum by (pod) (rate(container_cpu_usage_seconds_total{` + pods + `}[1m])))`,
		Memory:      `avg(sum by (pod) (container_memory_working_set_bytes{` + pods + `}))`,
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
	return c.CheckRollout(ctx, rollout, CheckOptions{})
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
		return Report{}, fmt.Errorf("instances of %s/%s revision %s: %w", svc.Namespace, svc.Name, rollout.Revision, err)
	}
	in := Inputs{Rollout: rollout, Now: now, Instances: instances, Checkpoint: opts.Checkpoint, BeforeEnd: opts.BeforeEnd}

	// Metrics only matter when there is a verdict to compute.
	var notes []string
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
			in.Current, notes = c.fetch(ctx, tpl, vars, from, to, 0)
			seasonal, _ := c.fetch(ctx, Templates{RequestRate: tpl.RequestRate}, vars, from, to, seasonalOffset)
			if len(seasonal.RequestRate) > 0 {
				in.Seasonal = &seasonal
			}
		}
	}

	r := Evaluate(cfg, in)
	r.Notes = append(r.Notes, notes...)
	return r, nil
}

type templateVars struct{ Namespace, Service, Revision string }

// safeName matches Kubernetes names and Compose service/project names.
// Template variables are interpolated into queries, so anything else is
// refused rather than escaped.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.-]*[a-zA-Z0-9])?$`)

// fetch runs every non-empty template over [from, to], shifted back by
// offset. Returned samples are shifted forward by the same offset so
// they line up with the current window.
func (c *Collector) fetch(ctx context.Context, tpl Templates, vars templateVars, from, to time.Time, offset time.Duration) (Signals, []string) {
	var sig Signals
	var notes []string
	for _, q := range []struct {
		name string
		tpl  string
		dst  *Series
	}{
		{"request rate", tpl.RequestRate, &sig.RequestRate},
		{"error rate", tpl.ErrorRate, &sig.ErrorRate},
		{"latency p95", tpl.LatencyP95, &sig.LatencyP95},
		{"CPU", tpl.CPU, &sig.CPU},
		{"memory", tpl.Memory, &sig.Memory},
	} {
		if q.tpl == "" {
			continue
		}
		query, err := render(q.tpl, vars)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s query: %v", q.name, err))
			continue
		}
		samples, err := c.Metrics.GetMetric(ctx, query, from.Add(-offset), to.Add(-offset), seriesStep)
		if err != nil {
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
	return sig, notes
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
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
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
