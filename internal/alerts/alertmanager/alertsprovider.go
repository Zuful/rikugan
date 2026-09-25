package alertmanager

import (
	"context"
	"fmt"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.AlertsProvider —
// the concept-level Alerts tools (list_active_alerts /
// get_alert_details) can route through this backend.
var _ provider.AlertsProvider = (*Provider)(nil)

// ListActiveAlerts returns all alerts whose AlertManager state is
// "active" (i.e. firing and not silenced). When service is non-empty it
// is added as a label filter: AlertManager's stream-style filter
// `service="<value>"`. Callers wanting to filter on a different label
// should pass that label in the filter raw — the concept-level tool
// hides that mechanism, so the helper here remains conservative.
func (p *Provider) ListActiveAlerts(ctx context.Context, service string) ([]provider.Alert, error) {
	var filters []string
	if service != "" {
		filters = append(filters, fmt.Sprintf(`service=%q`, service))
	}
	raw, err := p.client.ListAlerts(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("alertmanager list alerts: %w", err)
	}
	out := make([]provider.Alert, 0, len(raw))
	for _, a := range raw {
		if a.Status.State != "active" {
			continue
		}
		out = append(out, toProviderAlert(a))
	}
	return out, nil
}

// GetAlertDetails returns the alert matching the given fingerprint.
// AlertManager has no per-alert detail endpoint, so this re-fetches
// the active alert list and finds the match. Returns an error when
// the fingerprint isn't present in the current active set.
func (p *Provider) GetAlertDetails(ctx context.Context, alertID string) (provider.Alert, error) {
	raw, err := p.client.ListAlerts(ctx, nil)
	if err != nil {
		return provider.Alert{}, fmt.Errorf("alertmanager list alerts: %w", err)
	}
	for _, a := range raw {
		if a.Fingerprint == alertID {
			return toProviderAlert(a), nil
		}
	}
	return provider.Alert{}, fmt.Errorf("alertmanager: no alert with fingerprint %q", alertID)
}

// toProviderAlert projects an AlertManager-native Alert into the
// concept-level shape. The mapping is:
//   - ID       ← fingerprint
//   - Title    ← labels[alertname]
//   - Description ← annotations[summary] || annotations[description]
//   - Severity ← labels[severity]
//   - State    ← AM state remapped to firing|silenced|unknown
//   - Service  ← labels[service]
func toProviderAlert(a Alert) provider.Alert {
	desc := a.Annotations["summary"]
	if desc == "" {
		desc = a.Annotations["description"]
	}
	state := "unknown"
	switch a.Status.State {
	case "active":
		state = "firing"
	case "suppressed":
		state = "silenced"
	}
	return provider.Alert{
		ID:          a.Fingerprint,
		Title:       defaultString(a.Labels["alertname"], "(unnamed)"),
		Description: desc,
		Severity:    a.Labels["severity"],
		State:       state,
		StartedAt:   a.StartsAt,
		Service:     a.Labels["service"],
		Source:      "alertmanager",
		Extras: map[string]any{
			"labels":       a.Labels,
			"annotations":  a.Annotations,
			"generatorURL": a.GeneratorURL,
			"endsAt":       a.EndsAt,
			"silencedBy":   a.Status.SilencedBy,
			"inhibitedBy":  a.Status.InhibitedBy,
		},
	}
}
