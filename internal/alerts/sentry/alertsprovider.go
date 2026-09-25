package sentry

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Zuful/rikugan/pkg/provider"
)

// Compile-time check that *Provider satisfies provider.AlertsProvider —
// the concept-level Alerts tools (list_active_alerts /
// get_alert_details) can route through this backend.
var _ provider.AlertsProvider = (*Provider)(nil)

const defaultListLimit = 25

// ListActiveAlerts returns Sentry's unresolved issues for the given
// project slug. The concept-level `service` parameter is interpreted
// here as the Sentry project slug — the operator is responsible for
// knowing that mapping; the concept tool documents it.
//
// An empty service is rejected because the Sentry issues endpoint
// requires a project — there is no org-wide "list everything" query
// that returns a useful working set.
func (p *Provider) ListActiveAlerts(ctx context.Context, service string) ([]provider.Alert, error) {
	if service == "" {
		return nil, fmt.Errorf("sentry ListActiveAlerts requires a project slug as `service`")
	}
	raw, err := p.client.ListProjectIssues(ctx, service, "is:unresolved", defaultListLimit)
	if err != nil {
		return nil, fmt.Errorf("sentry list issues: %w", err)
	}
	out := make([]provider.Alert, 0, len(raw))
	for _, i := range raw {
		out = append(out, toProviderAlert(i))
	}
	return out, nil
}

// GetAlertDetails returns the full issue identified by its numeric
// Sentry ID (not the short ID like "WEB-A12").
func (p *Provider) GetAlertDetails(ctx context.Context, alertID string) (provider.Alert, error) {
	issue, err := p.client.GetIssue(ctx, alertID)
	if err != nil {
		return provider.Alert{}, fmt.Errorf("sentry get issue: %w", err)
	}
	return toProviderAlert(*issue), nil
}

// toProviderAlert projects a Sentry Issue into the concept-level
// Alert shape. State maps as: unresolved → firing, resolved → resolved,
// ignored → silenced, anything else → unknown.
func toProviderAlert(i Issue) provider.Alert {
	state := "unknown"
	switch i.Status {
	case "unresolved":
		state = "firing"
	case "resolved":
		state = "resolved"
	case "ignored":
		state = "silenced"
	}
	count, _ := strconv.Atoi(i.Count)
	extras := map[string]any{
		"shortID":   i.ShortID,
		"culprit":   i.Culprit,
		"count":     count,
		"userCount": i.UserCount,
		"firstSeen": i.FirstSeen,
		"lastSeen":  i.LastSeen,
		"permalink": i.Permalink,
		"metadata":  i.Metadata,
		"project":   i.Project.Slug,
	}
	if i.FirstRelease != nil {
		extras["firstRelease"] = i.FirstRelease.Version
	}
	desc := i.Metadata.Value
	if desc == "" {
		desc = i.Culprit
	}
	return provider.Alert{
		ID:          i.ID,
		Title:       i.Title,
		Description: desc,
		Severity:    i.Level,
		State:       state,
		StartedAt:   i.FirstSeen,
		Service:     i.Project.Slug,
		Source:      "sentry",
		Extras:      extras,
	}
}
