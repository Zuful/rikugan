package health

import (
	"context"
	"fmt"
	"time"

	"github.com/Zuful/rikugan/pkg/provider"
)

// SurveyResult measures, for one service, how often the volume guard
// would leave latency and error rate unevaluated: the share of windows
// of each checkpoint's length, over the surveyed period, that hold
// fewer than MinRequests requests. It depends only on traffic volume,
// not on rollouts, so it can be measured before any rollout happens.
type SurveyResult struct {
	Service      provider.Service
	MinRequests  float64
	Minutes      int                       // minutes of request-rate data found
	Insufficient map[time.Duration]float64 // checkpoint → share of windows below MinRequests, 0..1
}

// Survey fetches the service's request rate over [from, to] in one query
// and slides every checkpoint-length window across it.
func Survey(ctx context.Context, metrics provider.MetricsProvider, cfg Config, tpl Templates, svc provider.Service, from, to time.Time) (SurveyResult, error) {
	res := SurveyResult{Service: svc, MinRequests: cfg.MinRequests, Insufficient: map[time.Duration]float64{}}
	query, err := render(tpl.RequestRate, templateVars{Namespace: svc.Namespace, Service: svc.Name})
	if err != nil {
		return res, err
	}
	samples, err := metrics.GetMetric(ctx, query, from, to, seriesStep)
	if err != nil {
		return res, fmt.Errorf("request rate of %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	series, n := singleSeries(samples, 0)
	if n > 1 {
		return res, fmt.Errorf("request rate query for %s/%s returned %d series, expected one", svc.Namespace, svc.Name, n)
	}
	res.Minutes = len(series)
	for _, cp := range Checkpoints {
		res.Insufficient[cp] = insufficientShare(series, cp, cfg.MinRequests)
	}
	return res, nil
}

// insufficientShare slides a window of the given length over a
// per-minute rate series and returns the share of positions whose
// request count is below min. Minutes with no sample count as zero
// traffic: Prometheus returns no point when a rate has no data, which
// for a request counter means no requests.
func insufficientShare(series Series, window time.Duration, min float64) float64 {
	if len(series) == 0 {
		return 1
	}
	start, end := series[0].Time, series[len(series)-1].Time
	minutes := int(end.Sub(start)/seriesStep) + 1
	perMinute := make([]float64, minutes)
	for _, s := range series {
		i := int(s.Time.Sub(start) / seriesStep)
		if i >= 0 && i < minutes {
			perMinute[i] = s.Value * seriesStep.Seconds()
		}
	}

	width := int(window / seriesStep)
	if width > minutes {
		return 1
	}
	below, positions := 0, 0
	sum := 0.0
	for i, v := range perMinute {
		sum += v
		if i >= width {
			sum -= perMinute[i-width]
		}
		if i >= width-1 {
			positions++
			if sum < min {
				below++
			}
		}
	}
	return float64(below) / float64(positions)
}
