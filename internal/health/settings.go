package health

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Settings is the operator's override file. Every field is optional;
// what is left out keeps its default. Services are keyed
// "namespace/name" and inherit from defaults.
//
//	defaults:
//	  thresholds:
//	    min_requests: 100
//	  templates:
//	    request_rate: sum(rate(http_server_requests_seconds_count{namespace="{{.Namespace}}",app="{{.Service}}"}[1m]))
//	services:
//	  prod/checkout:
//	    thresholds:
//	      rollout_unavailable_grace: 90s
type Settings struct {
	Defaults Override            `json:"defaults"`
	Services map[string]Override `json:"services"`
}

// Override changes some thresholds and templates.
type Override struct {
	Thresholds Thresholds `json:"thresholds"`
	Templates  Templates  `json:"templates"`
}

// Thresholds mirrors Config with optional fields. Durations are written
// as Go durations ("90s", "2m").
type Thresholds struct {
	BeforeWindow *Duration `json:"before_window"`
	StaleAfter   *Duration `json:"stale_after"`

	MinRequests *float64 `json:"min_requests"`

	LatencyInvestigatePct *float64  `json:"latency_investigate_pct"`
	LatencyInvestigateAbs *Duration `json:"latency_investigate_abs"`
	LatencyDegradedPct    *float64  `json:"latency_degraded_pct"`
	LatencyDegradedAbs    *Duration `json:"latency_degraded_abs"`

	LatencyDegradedFromCheckpoint *Duration `json:"latency_degraded_from_checkpoint"`

	ErrorRatio             *float64 `json:"error_ratio"`
	ErrorInvestigateRate   *float64 `json:"error_investigate_rate"`
	ErrorInvestigateMinErr *float64 `json:"error_investigate_min_errors"`
	ErrorDegradedRate      *float64 `json:"error_degraded_rate"`
	ErrorDegradedMinErr    *float64 `json:"error_degraded_min_errors"`

	BluntRule        *bool    `json:"blunt_rule"`
	BluntErrorRate   *float64 `json:"blunt_error_rate"`
	BluntErrorMinErr *float64 `json:"blunt_error_min_errors"`

	SaturationInvestigatePct *float64  `json:"saturation_investigate_pct"`
	SaturationDegradedPct    *float64  `json:"saturation_degraded_pct"`
	SaturationFromCheckpoint *Duration `json:"saturation_from_checkpoint"`

	TrafficDropRatio *float64 `json:"traffic_drop_ratio"`
	TrafficRiseRatio *float64 `json:"traffic_rise_ratio"`
	TrafficZeroRatio *float64 `json:"traffic_zero_ratio"`

	UnavailableGrace        *Duration `json:"unavailable_grace"`
	RolloutUnavailableGrace *Duration `json:"rollout_unavailable_grace"`
}

// Duration reads "90s"-style strings.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"90s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// LoadSettings reads an override file. An empty path means no file:
// defaults everywhere.
func LoadSettings(path string) (Settings, error) {
	if path == "" {
		return Settings{}, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path
	if err != nil {
		return Settings{}, fmt.Errorf("read health settings: %w", err)
	}
	var s Settings
	if err := yaml.UnmarshalStrict(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("parse health settings %s: %w", path, err)
	}
	return s, nil
}

// For resolves a service's configuration: defaults, then the file's
// defaults section, then the service's own section.
func (s Settings) For(svc provider.Service) (Config, Templates) {
	cfg, tpl := DefaultConfig(), DefaultTemplates()
	s.Defaults.apply(&cfg, &tpl)
	if o, ok := s.Services[svc.Namespace+"/"+svc.Name]; ok {
		o.apply(&cfg, &tpl)
	}
	return cfg, tpl
}

func (o Override) apply(cfg *Config, tpl *Templates) {
	t := o.Thresholds
	setDur(&cfg.BeforeWindow, t.BeforeWindow)
	setDur(&cfg.StaleAfter, t.StaleAfter)
	setNum(&cfg.MinRequests, t.MinRequests)
	setNum(&cfg.LatencyInvestigatePct, t.LatencyInvestigatePct)
	setDur(&cfg.LatencyInvestigateAbs, t.LatencyInvestigateAbs)
	setNum(&cfg.LatencyDegradedPct, t.LatencyDegradedPct)
	setDur(&cfg.LatencyDegradedAbs, t.LatencyDegradedAbs)
	setDur(&cfg.LatencyDegradedFromCheckpoint, t.LatencyDegradedFromCheckpoint)
	setNum(&cfg.ErrorRatio, t.ErrorRatio)
	setNum(&cfg.ErrorInvestigateRate, t.ErrorInvestigateRate)
	setNum(&cfg.ErrorInvestigateMinErr, t.ErrorInvestigateMinErr)
	setNum(&cfg.ErrorDegradedRate, t.ErrorDegradedRate)
	setNum(&cfg.ErrorDegradedMinErr, t.ErrorDegradedMinErr)
	if t.BluntRule != nil {
		cfg.BluntRule = *t.BluntRule
	}
	setNum(&cfg.BluntErrorRate, t.BluntErrorRate)
	setNum(&cfg.BluntErrorMinErr, t.BluntErrorMinErr)
	setNum(&cfg.SaturationInvestigatePct, t.SaturationInvestigatePct)
	setNum(&cfg.SaturationDegradedPct, t.SaturationDegradedPct)
	setDur(&cfg.SaturationFromCheckpoint, t.SaturationFromCheckpoint)
	setNum(&cfg.TrafficDropRatio, t.TrafficDropRatio)
	setNum(&cfg.TrafficRiseRatio, t.TrafficRiseRatio)
	setNum(&cfg.TrafficZeroRatio, t.TrafficZeroRatio)
	setDur(&cfg.UnavailableGrace, t.UnavailableGrace)
	setDur(&cfg.RolloutUnavailableGrace, t.RolloutUnavailableGrace)

	setStr(&tpl.RequestRate, o.Templates.RequestRate)
	setStr(&tpl.ErrorRate, o.Templates.ErrorRate)
	setStr(&tpl.LatencyP95, o.Templates.LatencyP95)
	setStr(&tpl.CPU, o.Templates.CPU)
	setStr(&tpl.Memory, o.Templates.Memory)
}

func setNum(dst *float64, v *float64) {
	if v != nil {
		*dst = *v
	}
}

func setDur(dst *time.Duration, v *Duration) {
	if v != nil {
		*dst = time.Duration(*v)
	}
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
