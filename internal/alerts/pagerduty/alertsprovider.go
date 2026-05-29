package pagerduty

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.AlertsProvider —
// the concept-level Alerts tools (list_active_alerts /
// get_alert_details) can route through this backend.
var _ provider.AlertsProvider = (*Provider)(nil)

const activeAlertsLimit = 50

// ListActiveAlerts returns PagerDuty incidents that are currently
// triggered or acknowledged (i.e. not resolved). When service is
// non-empty it is interpreted as a PagerDuty service *name* and
// resolved via FindServiceByName; that's how the concept tool
// scopes per service.
func (p *Provider) ListActiveAlerts(ctx context.Context, service string) ([]provider.Alert, error) {
	params := url.Values{
		"statuses[]": {"triggered", "acknowledged"},
		"limit":      {fmt.Sprintf("%d", activeAlertsLimit)},
		"sort_by":    {"created_at:desc"},
	}
	if service != "" {
		svc, err := p.client.FindServiceByName(ctx, service)
		if err != nil {
			return nil, fmt.Errorf("pagerduty service lookup: %w", err)
		}
		params.Add("service_ids[]", svc.ID)
	}
	raw, err := p.client.ListIncidents(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("pagerduty list incidents: %w", err)
	}
	out := make([]provider.Alert, 0, len(raw))
	for _, i := range raw {
		out = append(out, toProviderAlert(i))
	}
	return out, nil
}

// GetAlertDetails fetches the full incident matching the given ID.
func (p *Provider) GetAlertDetails(ctx context.Context, alertID string) (provider.Alert, error) {
	inc, err := p.client.GetIncident(ctx, alertID)
	if err != nil {
		return provider.Alert{}, fmt.Errorf("pagerduty get incident: %w", err)
	}
	return toProviderAlert(*inc), nil
}

// toProviderAlert projects a PagerDuty Incident into the concept-level
// Alert shape. Status maps as: triggered → firing, acknowledged →
// acknowledged, resolved → resolved, anything else → unknown.
// Severity is taken from urgency (high → critical, low → warning),
// matching how PagerDuty users intuitively rank incidents.
func toProviderAlert(i Incident) provider.Alert {
	state := "unknown"
	switch i.Status {
	case "triggered":
		state = "firing"
	case "acknowledged":
		state = "acknowledged"
	case "resolved":
		state = "resolved"
	}
	severity := i.Urgency
	switch i.Urgency {
	case "high":
		severity = "critical"
	case "low":
		severity = "warning"
	}
	var assignee string
	if len(i.Assignments) > 0 {
		assignee = i.Assignments[0].Assignee.Summary
	}
	return provider.Alert{
		ID:          i.ID,
		Title:       i.Title,
		Description: i.Description,
		Severity:    severity,
		State:       state,
		StartedAt:   i.CreatedAt,
		Service:     i.Service.Summary,
		Source:      "pagerduty",
		Extras: map[string]any{
			"incidentNumber":  i.IncidentNumber,
			"urgency":         i.Urgency,
			"htmlURL":         i.HTMLURL,
			"lastStatusChange": i.LastStatusChange,
			"assignee":        assignee,
			"serviceID":       i.Service.ID,
		},
	}
}
